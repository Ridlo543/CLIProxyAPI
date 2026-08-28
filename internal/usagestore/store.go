// Package usagestore keeps per-request usage events for the management
// analytics API. Storage model:
//
//   - Hot window: the current and previous month are held fully in memory
//     (bounded ring) so default dashboard ranges answer without disk I/O.
//   - Permanent archive: every event is appended to a monthly JSONL file
//     (usage-YYYY-MM.jsonl). Nothing is ever truncated — history is kept
//     forever, and older months are read lazily (with a tiny LRU cache)
//     when a query reaches past the hot window.
package usagestore

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	hotMaxEvents   = 300_000 // bounded ring for the two hot months
	maxEventAge    = 0       // archived months are permanent
	archiveLRUSize = 4       // parsed month-files kept around
)

// Event is one settled provider request as seen by the usage pipeline.
type Event struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Alias     string    `json:"alias"`
	AuthIndex string    `json:"auth_index"`
	AuthType  string    `json:"auth_type"`
	APIKey    string    `json:"api_key,omitempty"`
	Account   string    `json:"account,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	LatencyMs   int64     `json:"latency_ms"`
	TTFTMs      int64     `json:"ttft_ms"`
	HandshakeMs int64     `json:"handshake_ms,omitempty"`
	Duration    int64     `json:"duration_ms,omitempty"`
	Failed    bool      `json:"failed"`
	StatusCod int       `json:"status_code,omitempty"`
	FailBody  string    `json:"fail_body,omitempty"`

	Input         int64   `json:"input_tokens"`
	Output        int64   `json:"output_tokens"`
	Cached        int64   `json:"cached_tokens"`
	CacheRead     int64   `json:"cache_read_tokens"`
	CacheCreation int64   `json:"cache_creation_tokens"`
	Reasoning     int64   `json:"reasoning_tokens"`
	Total         int64   `json:"total_tokens"`
	Cost          float64 `json:"cost"`
}

type Store struct {
	mu     sync.Mutex
	hot    []Event // current + previous month, oldest first
	nextID int64

	dir      string
	curMonth string
	file     *os.File
	writer   *bufio.Writer

	cacheMu sync.Mutex
	cache   map[string][]Event // archive file name -> parsed events
	lru     []string           // most-recently-used first
}

var global = &Store{}

func Default() *Store { return global }

func monthKey(t time.Time) string { return t.Format("2006-01") }
func fileName(m string) string    { return "usage-" + m + ".jsonl" }

// Configure points the store at a directory of monthly JSONL files and
// loads the hot window (current + previous month) into memory.
func (s *Store) Configure(dir string) error {
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.dir != "" || strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	global.dir = dir
	global.cache = make(map[string][]Event)
	global.lru = nil
	now := time.Now()
	for _, m := range []string{monthKey(now.AddDate(0, -1, 0)), monthKey(now)} {
		p := filepath.Join(dir, fileName(m))
		evs := readEventsFile(p)
		for _, ev := range evs {
			if ev.ID > global.nextID {
				global.nextID = ev.ID
			}
			global.hot = append(global.hot, ev)
		}
	}
	global.pruneLocked(now)
	f, err := os.OpenFile(filepath.Join(dir, fileName(monthKey(now))),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	global.curMonth = monthKey(now)
	global.file = f
	global.writer = bufio.NewWriterSize(f, 32*1024)
	log.Infof("usagestore: %d hot events loaded from %s", len(global.hot), dir)
	return nil
}

// Configure is the package-level helper used at startup.
func Configure(dir string) error { return Default().Configure(dir) }

// Close flushes and releases open handles (shutdown/tests).
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		err := s.file.Close()
		s.file, s.writer = nil, nil
		return err
	}
	return nil
}

// Add inserts an event into the hot window and appends it to its month file.
func (s *Store) Add(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	ev.ID = s.nextID
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.Cost == 0 {
		ev.Cost = CalculateCost(ev.Model, CostTokens{
			Input:         ev.Input,
			Output:        ev.Output,
			Cached:        ev.CacheRead,
			CacheCreation: ev.CacheCreation,
			Reasoning:     ev.Reasoning,
		})
	}
	s.hot = append(s.hot, ev)
	m := monthKey(ev.Timestamp)
	if m != s.curMonth && s.dir != "" {
		s.rollToLocked(m)
	} else {
		s.persistLocked(ev)
	}
	s.pruneLocked(time.Now())
}

// Query returns events within [from, to], oldest first. Ranges reaching
// beyond the hot window transparently read archived month files.
func (s *Store) Query(from, to time.Time) []Event {
	out := make([]Event, 0, 1024)

	s.mu.Lock()
	for _, ev := range s.hot {
		if !ev.Timestamp.Before(from) && !ev.Timestamp.After(to) {
			out = append(out, ev)
		}
	}
	dir := s.dir
	s.mu.Unlock()

	// Archive months strictly older than the previous month are not in
	// memory; read them lazily through the LRU-cached parser. Only months
	// that actually have an archive file on disk are visited.
	nowM := monthKey(time.Now())
	prevM := monthKey(time.Now().AddDate(0, -1, 0))
	if dir != "" {
		for _, m := range archiveMonthsOnDisk(dir, from, to, nowM, prevM) {
			out = append(out, s.archived(filepath.Join(dir, fileName(m)))...)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// archiveMonthsOnDisk lists month keys that (a) exist as archive files and
// (b) overlap [from, to], excluding the two hot months.
func archiveMonthsOnDisk(dir string, from, to time.Time, hotMonths ...string) []string {
	skip := make(map[string]bool, len(hotMonths))
	for _, m := range hotMonths {
		skip[m] = true
	}
	lo, hi := monthKey(from), monthKey(to)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "usage-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		m := strings.TrimSuffix(strings.TrimPrefix(n, "usage-"), ".jsonl")
		if len(m) != 7 || skip[m] || m < lo || m > hi {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (s *Store) pruneLocked(now time.Time) {
	// Hot window is bounded by count only; month boundaries decide what is
	// served from memory vs archive, so nothing is dropped by age here.
	if overflow := len(s.hot) - hotMaxEvents; overflow > 0 {
		s.hot = append(s.hot[:0], s.hot[overflow:]...)
	}
}

func (s *Store) persistLocked(ev Event) {
	if s.writer == nil {
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := s.writer.Write(append(line, '\n')); err != nil {
		return
	}
	_ = s.writer.Flush()
}

func (s *Store) rollToLocked(month string) {
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		_ = s.file.Close()
	}
	f, err := os.OpenFile(filepath.Join(s.dir, fileName(month)),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Errorf("usagestore: roll to %s failed: %v", month, err)
		s.file, s.writer = nil, nil
		return
	}
	s.curMonth = month
	s.file = f
	s.writer = bufio.NewWriterSize(f, 32*1024)
}

// archived returns parsed events for an archive file, caching the result.
// Archive events get stable sequential IDs that continue after the hot
// window's IDs so cursor pagination stays consistent for the process
// lifetime.
func (s *Store) archived(path string) []Event {
	s.cacheMu.Lock()
	if evs, ok := s.cache[path]; ok {
		s.cacheMu.Unlock()
		return evs
	}
	s.cacheMu.Unlock()

	evs := readEventsFile(path)
	s.mu.Lock()
	for i := range evs {
		s.nextID++
		evs[i].ID = s.nextID
	}
	base := s.nextID
	s.mu.Unlock()
	s.cacheMu.Lock()
	s.cache[path] = evs
	s.lru = append([]string{path}, s.lru...)
	if len(s.lru) > archiveLRUSize {
		for _, victim := range s.lru[archiveLRUSize:] {
			delete(s.cache, victim)
		}
		s.lru = s.lru[:archiveLRUSize]
	}
	s.cacheMu.Unlock()
	_ = base
	return evs
}

func readEventsFile(path string) []Event {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := make([]Event, 0, 4096)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Timestamp.IsZero() {
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// RecordFromUsage converts a pipeline usage record into a stored event.
func RecordFromUsage(record coreusage.Record, statusCode int) Event {
	detail := record.Detail
	ev := Event{
		Timestamp:     record.RequestedAt,
		Provider:      record.Provider,
		Model:         record.Model,
		Alias:         record.Alias,
		AuthIndex:     record.AuthIndex,
		AuthType:      record.AuthType,
		APIKey:        record.APIKey,
		LatencyMs:     record.Latency.Milliseconds(),
		TTFTMs:        record.TTFT.Milliseconds(),
		HandshakeMs:   record.Handshake.Milliseconds(),
		Duration:      record.Latency.Milliseconds(),
		Input:         detail.InputTokens,
		Output:        detail.OutputTokens,
		Cached:        detail.CachedTokens,
		CacheRead:     detail.CacheReadTokens,
		CacheCreation: detail.CacheCreationTokens,
		Reasoning:     detail.ReasoningTokens,
		Total:         detail.TotalTokens,
	}
	if record.Failed || statusCode >= 400 {
		ev.Failed = true
		ev.StatusCod = statusCode
		ev.FailBody = record.Fail.Body
	}
	if ev.StatusCod == 0 && !ev.Failed {
		ev.StatusCod = 200
	}
	if ev.Failed && ev.StatusCod <= 0 {
		ev.StatusCod = 502
	}
	return ev
}
