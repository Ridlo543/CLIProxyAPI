// Package usagestore keeps an in-memory ring of per-request usage events so
// the management API can serve full analytics natively, without any external
// aggregation service. Events are additionally appended to a JSONL file so
// history survives router restarts.
package usagestore

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	defaultMaxEvents = 100_000
	maxEventAge      = 30 * 24 * time.Hour
	maxFileSizeBytes = 64 << 20 // rotate the JSONL above this size
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
	Endpoint  string    `json:"endpoint,omitempty"`
	LatencyMs int64     `json:"latency_ms"`
	TTFTMs    int64     `json:"ttft_ms"`
	Duration  int64     `json:"duration_ms,omitempty"`
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

// Store is a bounded, thread-safe event ring backed by an append-only JSONL
// file so analytics survive restarts.
type Store struct {
	mu     sync.Mutex
	events []Event
	nextID int64

	filePath string
	file     *os.File
	writer   *bufio.Writer
}

var global = &Store{}

// Default returns the process-wide store.
func Default() *Store { return global }

// Add inserts an event into the ring and persists it when configured.
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
	s.events = append(s.events, ev)
	s.pruneLocked(time.Now())
	s.persistLocked(ev)
}

// Query returns the events whose timestamp falls within [from, to], oldest
// first. The slice is a copy and safe to iterate without the lock.
func (s *Store) Query(from, to time.Time) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.events))
	for _, ev := range s.events {
		if !ev.Timestamp.Before(from) && !ev.Timestamp.After(to) {
			out = append(out, ev)
		}
	}
	return out
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-maxEventAge)
	drop := 0
	for drop < len(s.events) && s.events[drop].Timestamp.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		s.events = append(s.events[:0], s.events[drop:]...)
	}
	if overflow := len(s.events) - defaultMaxEvents; overflow > 0 {
		s.events = append(s.events[:0], s.events[overflow:]...)
	}
}

// Configure loads prior events from the given JSONL path and keeps appending
// new ones there. Safe to call once during startup; a failure disables
// persistence without affecting in-memory analytics.
func (s *Store) Configure(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filePath != "" || strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := s.loadLocked(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.filePath = path
	s.file = f
	s.writer = bufio.NewWriterSize(f, 32*1024)
	log.Infof("usagestore: %d historical events loaded from %s", len(s.events), path)
	return nil
}

func (s *Store) loadLocked(path string) error {
	for _, p := range []string{rotatePath(path), path} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(line), &ev) != nil || ev.Timestamp.IsZero() {
				continue
			}
			if ev.ID > s.nextID {
				s.nextID = ev.ID
			}
			s.events = append(s.events, ev)
		}
	}
	s.pruneLocked(time.Now())
	return nil
}

func rotatePath(path string) string { return path + ".1" }

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
	if err := s.writer.Flush(); err != nil {
		return
	}
	if st, statErr := s.file.Stat(); statErr == nil && st.Size() > maxFileSizeBytes {
		s.rotateLocked()
	}
}

func (s *Store) rotateLocked() {
	if s.file != nil {
		_ = s.file.Close()
	}
	_ = os.Rename(s.filePath, rotatePath(s.filePath))
	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		s.file = nil
		s.writer = nil
		log.Errorf("usagestore: rotation failed: %v", err)
		return
	}
	s.file = f
	s.writer = bufio.NewWriterSize(f, 32*1024)
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
		// Upstream never returned a status (transport-level failure).
		ev.StatusCod = 502
	}
	return ev
}

// Configure wires persistence onto the default store (package-level helper).
func Configure(path string) error { return global.Configure(path) }

// Close flushes and releases the persistence file. Used on shutdown/tests.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		s.writer = nil
		return err
	}
	return nil
}
