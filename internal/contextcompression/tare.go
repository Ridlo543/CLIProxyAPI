package contextcompression

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const tareSchemaVersion = "tare-block-v1"

type cacheEntry struct {
	key, value string
	bytes      int
}
type tareIdentity struct {
	available                           bool
	reason, version, sha256, manifestID string
}
type identityCall struct {
	ready    chan struct{}
	identity tareIdentity
}

type tareEngine struct {
	semaphore          chan struct{}
	mu                 sync.Mutex
	cache              []cacheEntry
	cacheBytes         int
	identities         map[string]*identityCall
	currentIdentityKey string
	children           map[*os.Process]struct{}
	active, queued     int
	shutdownCh         chan struct{}
	shutdownOnce       sync.Once
	closed             bool
	admitted           int
	drainChanged       chan struct{}
	identityBarrier    func()
	readIdentityBinary func(string) ([]byte, error)
	runIdentityVersion func(context.Context, config.TAREStructuralConfig) ([]byte, string)
}

func newTAREEngine() *tareEngine {
	e := &tareEngine{semaphore: make(chan struct{}, 1), identities: map[string]*identityCall{}, children: map[*os.Process]struct{}{}, shutdownCh: make(chan struct{}), drainChanged: make(chan struct{})}
	e.readIdentityBinary = os.ReadFile
	e.runIdentityVersion = func(ctx context.Context, cfg config.TAREStructuralConfig) ([]byte, string) {
		// Budget follows the configured process timeout: under -race on slow
		// runners a hardcoded 1s starves the version probe and poisons identity.
		return e.run(ctx, cfg, []string{"--version"}, nil, runLimits{timeoutMS: cfg.ProcessTimeoutMS, input: 1, stdout: 256, stderr: 1024})
	}
	return e
}

func (e *tareEngine) signalDrainLocked() { close(e.drainChanged); e.drainChanged = make(chan struct{}) }
func (e *tareEngine) admit() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.admitted++
	e.signalDrainLocked()
	return true
}
func (e *tareEngine) finishAdmission() {
	e.mu.Lock()
	e.admitted--
	e.signalDrainLocked()
	e.mu.Unlock()
}

func (e *tareEngine) compress(ctx context.Context, slots []slot, cfg config.TAREStructuralConfig, stats *Stats) bool {
	if !e.admit() {
		stats.Reason = "shutdown"
		return false
	}
	defer e.finishAdmission()
	identity := e.identity(ctx, cfg)
	stats.ManifestID = safeTelemetryManifest(identity.manifestID)
	stats.Version = safeTelemetryVersion(identity.version)
	if !identity.available {
		stats.Reason = identity.reason
		return false
	}
	e.enforceCacheLimits(cfg)
	type staged struct {
		s   slot
		out string
	}
	pending := make([]staged, 0, len(slots))
	writes := make([]cacheEntry, 0)
	for _, s := range slots {
		input := marshalBlock(s)
		if len(input) > cfg.InputLimitBytes {
			stats.Reason = "input_limit"
			return false
		}
		contentHash := cacheKey(s.text)
		key := cacheKey(tareSchemaVersion, identity.sha256, identity.version, "tare_structural", "default", identity.manifestID, s.class, contentHash)
		out, ok := e.get(key)
		if ok {
			stats.CacheHits++
		} else {
			raw, reason := e.run(ctx, cfg, []string{"compress"}, input, runLimits{timeoutMS: cfg.ProcessTimeoutMS, input: cfg.InputLimitBytes, stdout: cfg.StdoutLimitBytes, stderr: cfg.StderrLimitBytes})
			if reason != "" {
				stats.Reason = reason
				return false
			}
			raw = bytes.TrimSuffix(raw, []byte("\n"))
			if !utf8.Valid(raw) {
				stats.Reason = "invalid_utf8"
				return false
			}
			out = string(raw)
			writes = append(writes, cacheEntry{key, out, len(raw)})
		}
		before, after := len([]byte(s.text)), len([]byte(out))
		if out == "" || after >= before {
			stats.Reason = "not_smaller"
			return false
		}
		stats.BytesBefore += before
		stats.BytesAfter += after
		pending = append(pending, staged{s, out})
	}
	for _, v := range writes {
		e.put(v, cfg)
	}
	for _, p := range pending {
		p.s.write(p.out)
	}
	stats.Compressed = len(pending)
	return true
}

var telemetryVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
var telemetryManifestRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func safeTelemetryVersion(v string) string {
	if len(v) <= 16 && telemetryVersionRE.MatchString(v) {
		return v
	}
	return ""
}
func safeTelemetryManifest(v string) string {
	if telemetryManifestRE.MatchString(v) {
		return v
	}
	return ""
}

func identityConfigKey(cfg config.TAREStructuralConfig) string {
	versions := append([]string(nil), cfg.AllowedVersions...)
	sort.Strings(versions)
	return cacheKey(cfg.BinaryPath, strings.ToLower(cfg.SHA256), strings.Join(versions, "\x00"), cfg.ManifestID)
}
func (e *tareEngine) identity(ctx context.Context, cfg config.TAREStructuralConfig) tareIdentity {
	key := identityConfigKey(cfg)
	e.mu.Lock()
	if e.currentIdentityKey != key {
		e.currentIdentityKey = key
		e.cache = nil
		e.cacheBytes = 0
		for oldKey, old := range e.identities {
			select {
			case <-old.ready:
				delete(e.identities, oldKey)
			default:
			}
		}
	}
	if existing := e.identities[key]; existing != nil {
		e.mu.Unlock()
		select {
		case <-existing.ready:
			return existing.identity
		case <-ctx.Done():
			return tareIdentity{reason: "aborted", manifestID: cfg.ManifestID}
		}
	}
	call := &identityCall{ready: make(chan struct{})}
	if e.closed {
		e.mu.Unlock()
		return tareIdentity{reason: "shutdown", manifestID: cfg.ManifestID}
	}
	// The shared initializer has its own admission because request waiters may
	// cancel while the process-scoped identity operation must remain drainable.
	e.admitted++
	e.signalDrainLocked()
	e.identities[key] = call
	e.mu.Unlock()
	go func() {
		defer e.finishAdmission()
		if e.identityBarrier != nil {
			e.identityBarrier()
		}
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			call.identity = tareIdentity{reason: "shutdown", manifestID: cfg.ManifestID}
			close(call.ready)
			return
		}
		identityCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		call.identity = e.resolveIdentity(identityCtx, cfg)
		close(call.ready)
	}()
	select {
	case <-call.ready:
		return call.identity
	case <-ctx.Done():
		return tareIdentity{reason: "aborted", manifestID: cfg.ManifestID}
	}
}
func (e *tareEngine) resolveIdentity(ctx context.Context, cfg config.TAREStructuralConfig) tareIdentity {
	id := tareIdentity{manifestID: cfg.ManifestID}
	if cfg.BinaryPath == "" {
		id.reason = "unavailable"
		return id
	}
	data, err := e.readIdentityBinary(cfg.BinaryPath)
	if err != nil {
		id.reason = "unavailable"
		return id
	}
	if cfg.SHA256 == "" {
		id.reason = "checksum_required"
		return id
	}
	sum := sha256.Sum256(data)
	id.sha256 = hex.EncodeToString(sum[:])
	if !strings.EqualFold(id.sha256, cfg.SHA256) {
		id.reason = "checksum_mismatch"
		return id
	}
	raw, reason := e.runIdentityVersion(ctx, cfg)
	if reason != "" {
		if reason != "timeout" && reason != "aborted" && reason != "shutdown" {
			reason = "version_mismatch"
		}
		id.reason = reason
		return id
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || fields[0] != "tare" {
		id.reason = "version_mismatch"
		return id
	}
	for _, allowed := range cfg.AllowedVersions {
		if fields[1] == allowed {
			id.available = true
			id.reason = "available"
			id.version = allowed
			return id
		}
	}
	id.reason = "version_mismatch"
	return id
}

type runLimits struct{ timeoutMS, input, stdout, stderr int }

func (e *tareEngine) run(ctx context.Context, cfg config.TAREStructuralConfig, args []string, input []byte, limits runLimits) ([]byte, string) {
	if len(input) > limits.input {
		return nil, "input_limit"
	}
	queueCtx, cancelQueue := context.WithTimeout(ctx, time.Duration(cfg.QueueTimeoutMS)*time.Millisecond)
	defer cancelQueue()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, "shutdown"
	}
	e.queued++
	e.signalDrainLocked()
	e.mu.Unlock()
	select {
	case e.semaphore <- struct{}{}:
		e.mu.Lock()
		e.queued--
		if e.closed {
			e.signalDrainLocked()
			e.mu.Unlock()
			<-e.semaphore
			return nil, "shutdown"
		}
		e.active++
		e.signalDrainLocked()
		e.mu.Unlock()
		defer func() { <-e.semaphore; e.mu.Lock(); e.active--; e.signalDrainLocked(); e.mu.Unlock() }()
	case <-queueCtx.Done():
		e.mu.Lock()
		e.queued--
		e.signalDrainLocked()
		e.mu.Unlock()
		if ctx.Err() != nil {
			return nil, "aborted"
		}
		return nil, "queue_timeout"
	case <-e.shutdownCh:
		e.mu.Lock()
		e.queued--
		e.signalDrainLocked()
		e.mu.Unlock()
		return nil, "shutdown"
	}
	procCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.timeoutMS)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(procCtx, cfg.BinaryPath, args...)
	cmd.Stdin = bytes.NewReader(input)
	stdout := &limitBuffer{limit: limits.stdout}
	stderr := &limitBuffer{limit: limits.stderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Hold the state lock across Start+registration. Shutdown either closes
	// admission first (so no process starts), or sees and kills this child.
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, "shutdown"
	}
	if err := cmd.Start(); err != nil {
		e.mu.Unlock()
		return nil, "spawn_error"
	}
	e.children[cmd.Process] = struct{}{}
	e.signalDrainLocked()
	e.mu.Unlock()
	stdout.setKill(cmd.Process)
	stderr.setKill(cmd.Process)
	err := cmd.Wait()
	e.mu.Lock()
	delete(e.children, cmd.Process)
	e.signalDrainLocked()
	e.mu.Unlock()
	select {
	case <-e.shutdownCh:
		return nil, "shutdown"
	default:
	}
	if ctx.Err() != nil {
		return nil, "aborted"
	}
	if errors.Is(procCtx.Err(), context.DeadlineExceeded) {
		return nil, "timeout"
	}
	if stdout.exceeded {
		return nil, "stdout_limit"
	}
	if stderr.exceeded {
		return nil, "stderr_limit"
	}
	if err != nil {
		return nil, "nonzero"
	}
	return stdout.buf, ""
}

type limitBuffer struct {
	mu       sync.Mutex
	buf      []byte
	limit    int
	exceeded bool
	process  *os.Process
}

func (b *limitBuffer) setKill(process *os.Process) {
	b.mu.Lock()
	b.process = process
	exceeded := b.exceeded
	b.mu.Unlock()
	if exceeded && process != nil {
		_ = process.Kill()
	}
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.buf)+n > b.limit {
		b.exceeded = true
		remaining := b.limit - len(b.buf)
		if remaining > 0 {
			b.buf = append(b.buf, p[:remaining]...)
		}
		if b.process != nil {
			_ = b.process.Kill()
		}
		return n, nil
	}
	b.buf = append(b.buf, p...)
	return n, nil
}
func cacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len([]byte(p)))
		io.WriteString(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func (e *tareEngine) get(key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, v := range e.cache {
		if v.key == key {
			copy(e.cache[i:], e.cache[i+1:])
			e.cache[len(e.cache)-1] = v
			return v.value, true
		}
	}
	return "", false
}
func (e *tareEngine) put(v cacheEntry, cfg config.TAREStructuralConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, old := range e.cache {
		if old.key == v.key {
			e.cacheBytes -= old.bytes
			e.cache = append(e.cache[:i], e.cache[i+1:]...)
			break
		}
	}
	if v.bytes <= cfg.CacheBytes {
		e.cache = append(e.cache, v)
		e.cacheBytes += v.bytes
	}
	e.trimCacheLocked(cfg)
}
func (e *tareEngine) enforceCacheLimits(cfg config.TAREStructuralConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trimCacheLocked(cfg)
}
func (e *tareEngine) trimCacheLocked(cfg config.TAREStructuralConfig) {
	for len(e.cache) > cfg.CacheEntries || e.cacheBytes > cfg.CacheBytes {
		e.cacheBytes -= e.cache[0].bytes
		e.cache = e.cache[1:]
	}
}
func (e *tareEngine) stats() (active, queued, children, entries, cacheBytes int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active, e.queued, len(e.children), len(e.cache), e.cacheBytes
}
func (e *tareEngine) shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		close(e.shutdownCh)
		e.signalDrainLocked()
		e.mu.Unlock()
	})
	for {
		e.mu.Lock()
		children := make([]*os.Process, 0, len(e.children))
		for child := range e.children {
			children = append(children, child)
		}
		active, queued, admitted := e.active, e.queued, e.admitted
		changed := e.drainChanged
		e.mu.Unlock()
		for _, child := range children {
			_ = child.Kill()
		}
		if admitted == 0 && active == 0 && queued == 0 && len(children) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (e *tareEngine) reset() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = e.shutdown(shutdownCtx)
	cancel()
	e.mu.Lock()
	e.cache = nil
	e.cacheBytes = 0
	e.identities = map[string]*identityCall{}
	e.currentIdentityKey = ""
	e.shutdownCh = make(chan struct{})
	e.shutdownOnce = sync.Once{}
	e.closed = false
	e.admitted = 0
	e.drainChanged = make(chan struct{})
	e.mu.Unlock()
}
