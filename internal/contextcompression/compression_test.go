package contextcompression

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var testRuntime = NewRuntime()
var defaultTARE = testRuntime.tare

func Apply(ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, optOut bool) ([]byte, Stats) {
	return testRuntime.Apply(ctx, raw, cfg, optOut)
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "compress") {
		fakeTAREMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeTAREMain() {
	if count := os.Getenv("FAKE_TARE_COUNT_FILE"); count != "" {
		f, _ := os.OpenFile(count, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if f != nil {
			_, _ = f.WriteString(os.Args[1] + "\n")
			_ = f.Close()
		}
	}
	if os.Args[1] == "--version" {
		if os.Getenv("FAKE_TARE_VERSION_DELAY") != "" {
			time.Sleep(150 * time.Millisecond)
		}
		fmt.Printf("tare %s\n", envOr("FAKE_TARE_VERSION", "0.2.0"))
		return
	}
	input, _ := io.ReadAll(os.Stdin)
	mode := os.Getenv("FAKE_TARE_MODE")
	switch mode {
	case "nonzero":
		os.Exit(2)
	case "timeout":
		time.Sleep(5 * time.Second)
		return
	case "stdout-cap", "stdout-cap-block":
		fmt.Print(strings.Repeat("x", 2*1024*1024))
		if mode == "stdout-cap-block" {
			time.Sleep(5 * time.Second)
		}
		return
	case "stderr-cap", "stderr-cap-block":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 128*1024))
		if mode == "stderr-cap-block" {
			time.Sleep(5 * time.Second)
		}
		return
	case "invalid-utf8":
		_, _ = os.Stdout.Write([]byte{0xff, 0xfe})
		return
	}
	var blocks []map[string]string
	if json.Unmarshal(input, &blocks) != nil || len(blocks) != 1 {
		os.Exit(3)
	}
	text := blocks[0]["text"]
	if mode == "same" {
		fmt.Print(text)
		return
	}
	text = strings.TrimPrefix(text, "[[FAKE_TARE_COMPRESS]]")
	text = strings.TrimSuffix(text, "[[/FAKE_TARE_COMPRESS]]")
	fmt.Print(text)
}
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fakeTAREConfig(t *testing.T) config.ContextCompressionConfig {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return config.ContextCompressionConfig{Engine: config.ContextCompressionTARE, MinBytes: 500, RawCapBytes: 10 * 1024 * 1024, TARE: config.TAREStructuralConfig{BinaryPath: binary, SHA256: fmt.Sprintf("%x", sum), AllowedVersions: []string{"0.2.0"}, ManifestID: "tare-cli-test", ProcessTimeoutMS: 300, QueueTimeoutMS: 50, InputLimitBytes: 1024*1024 + 1024, StdoutLimitBytes: 1024 * 1024, StderrLimitBytes: 64 * 1024, GlobalConcurrency: 1, CacheEntries: 128, CacheBytes: 16 * 1024 * 1024}}
}
func marked(v string) string { return "[[FAKE_TARE_COMPRESS]]" + v + "[[/FAKE_TARE_COMPRESS]]" }

func TestCollectSlotsPreservesErrorsMediaAndObjects(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"tool","content":"openai"},{"role":"tool","content":[{"type":"text","text":"array"},{"type":"image","source":{"data":"secret"}}]},{"type":"function_call_output","output":[{"type":"input_text","text":"response"},{"nested":true}]},{"role":"user","content":[{"type":"tool_result","content":"claude"},{"type":"tool_result","is_error":true,"content":"error"}]}],"contents":[{"parts":[{"functionResponse":{"response":{"result":"gemini"}}}]}],"conversationState":{"history":[{"userInputMessage":{"userInputMessageContext":{"toolResults":[{"status":"success","content":[{"text":"kiro"}]},{"status":"error","content":[{"text":"kiro-error"}]}]}}}]}}`)
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	slots := collectSlots(body, 1024*1024)
	got := make([]string, len(slots))
	for i, s := range slots {
		got[i] = s.text
	}
	want := []string{"openai", "array", "response", "claude", "kiro", "gemini"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("slots=%q want=%q", got, want)
	}
	out, _ := json.Marshal(body)
	if !strings.Contains(string(out), `"data":"secret"`) || !strings.Contains(string(out), `"content":"error"`) || !strings.Contains(string(out), `"nested":true`) {
		t.Fatalf("preservation failed: %s", out)
	}
}

func TestRTKApplyFailOpenAndOptOut(t *testing.T) {
	lines := make([]string, 600)
	for i := range lines {
		lines[i] = "same repeated build output line"
	}
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": strings.Join(lines, "\n")}}})
	cfg := config.ContextCompressionConfig{Engine: config.ContextCompressionRTK, MinBytes: 500, RawCapBytes: 10 * 1024 * 1024}
	out, stats := Apply(context.Background(), raw, cfg, false)
	if !stats.Applied || len(out) >= len(raw) || stats.Compressed != 1 {
		t.Fatalf("stats=%+v sizes=%d/%d", stats, len(out), len(raw))
	}
	bypass, bypassStats := Apply(context.Background(), raw, cfg, true)
	if string(bypass) != string(raw) || bypassStats.Reason != "opt_out" {
		t.Fatalf("bypass=%+v", bypassStats)
	}
	invalid, invalidStats := Apply(context.Background(), []byte("{"), cfg, false)
	if string(invalid) != "{" || invalidStats.Reason != "invalid_json" {
		t.Fatalf("invalid=%+v", invalidStats)
	}
}

func TestTAREAtomicFailOpenWhenUnavailable(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"tool","content":"abcdefgh"},{"role":"tool","content":"12345678"}]}`)
	cfg := config.ContextCompressionConfig{Engine: config.ContextCompressionTARE, MinBytes: 1, RawCapBytes: 1024, TARE: config.TAREStructuralConfig{BinaryPath: "missing", ProcessTimeoutMS: 100, QueueTimeoutMS: 10, InputLimitBytes: 2048, StdoutLimitBytes: 1024, StderrLimitBytes: 256, GlobalConcurrency: 1, CacheEntries: 2, CacheBytes: 1024}}
	out, stats := Apply(context.Background(), raw, cfg, false)
	if string(out) != string(raw) || stats.Reason != "unavailable" {
		t.Fatalf("stats=%+v out=%s", stats, out)
	}
}

func TestRTKDetectionParityAndLateGrepFalsePositive(t *testing.T) {
	grepInput := strings.Repeat("src/a.go:12:match\n", 20)
	filter := autoDetectFilter(grepInput)
	if filter == nil || filter.name != "grep" {
		t.Fatalf("filter=%v", filter)
	}
	out := filter.apply(grepInput)
	if !strings.Contains(out, "  +10") {
		t.Fatalf("missing omission marker: %s", out)
	}
	late := strings.Join([]string{"ordinary log one", "ordinary log two", "ordinary log three", "ordinary log four", "ordinary log five", "ordinary log six", "late.go:9:not a grep listing"}, "\n")
	filter = autoDetectFilter(late)
	if filter == nil || filter.name != "dedup-log" {
		t.Fatalf("late grep false positive: %#v", filter)
	}
	for _, tc := range []struct{ input, name string }{{"commit 0123456789abcdef\nAuthor: A\nDate: now\n\n    subject", "git-log"}, {"diff --git a/a b/a\n@@ -1 +1 @@\n-a\n+b", "git-diff"}, {"On branch main\nnothing to commit", "git-status"}, {"Compiling foo\nFinished dev", "build-output"}, {"./a/x\n./a/y\n./b/z", "find"}, {".\n├── a\n└── b", "tree"}, {"Result of search in '.' (total 2 files):\n- a/x\n- a/y", "search-list"}} {
		f := autoDetectFilter(tc.input)
		if f == nil || f.name != tc.name {
			t.Errorf("%s detected %#v", tc.name, f)
		}
	}
	gitLog := gitLogFilter("commit 0123456789abcdef\nAuthor: A\nDate: now\n\n    subject\ndiff --git a/a b/a\n+secret\n" + strings.Repeat("long discarded commit body line\n", 20))
	if !strings.Contains(gitLog, "Subject: subject") || !strings.Contains(gitLog, "diff body omitted") || strings.Contains(gitLog, "+secret") {
		t.Fatalf("git log=%s", gitLog)
	}
	if f := autoDetectFilter("application commit abcdefg failed\nordinary\nordinary\nordinary\nordinary"); f != nil && f.name == "git-log" {
		t.Fatalf("git log false positive: %#v", f)
	}
}

func TestGitLogGraphSubjectsAndWindowsFindParity(t *testing.T) {
	graph := "* commit 0123456789abcdef\n| Author: A\n| Date: now\n|\n|     graph subject\n" + strings.Repeat("| body body body body body body body body body body body body body body\n", 10)
	if !isGitLogSubject("|     graph subject") {
		t.Fatal("graph subject predicate failed")
	}
	out := gitLogFilter(graph)
	if !strings.Contains(out, "Subject: |     graph subject") {
		t.Fatalf("graph=%s", out)
	}
	merge := "*   commit fedcba9876543210\n|\\  Author: A\n| | Date: now\n| |\n| |     merge subject\n" + strings.Repeat("| | discarded discarded discarded discarded discarded discarded\n", 10)
	out = gitLogFilter(merge)
	if !strings.Contains(out, "Subject: | |     merge subject") {
		t.Fatalf("merge=%s", out)
	}
	windows := "C:\\Users\\me\\src\\a.go\nC:\\Users\\me\\src\\b.go\nD:\\work\\c.go:10"
	f := autoDetectFilter(windows)
	if f == nil || f.name != "find" {
		t.Fatalf("windows filter=%#v", f)
	}
	out = f.apply(windows)
	if !strings.Contains(out, "C:/Users/me/src/") || !strings.Contains(out, "a.go") {
		t.Fatalf("windows=%s", out)
	}
}

func TestFilterCategoryFixtures(t *testing.T) {
	numbered := make([]string, 260)
	plain := make([]string, 260)
	for i := range numbered {
		numbered[i] = fmt.Sprintf("%d|line", i+1)
		plain[i] = fmt.Sprintf("plain-%d", i)
	}
	cases := []struct{ name, input, contains string }{{"ls", "total 3\n-rw-r--r-- 1 u g 2048 Jan 1 2024 a.go\n-rw-r--r-- 1 u g 10 Jan 1 2024 b.go\ndrwxr-xr-x 1 u g 0 Jan 1 2024 src", "Summary:"}, {"dedup-log", "a\na\na\nb\nc\nd\ne", "duplicate lines"}}
	for _, tc := range cases {
		f := autoDetectFilter(tc.input)
		if f == nil && tc.name == "smart-truncate" {
			f = &namedFilter{"smart-truncate", smartTruncateFilter}
		}
		if f == nil || f.name != tc.name {
			t.Errorf("%s detected %#v", tc.name, f)
			continue
		}
		if out := f.apply(tc.input); !strings.Contains(out, tc.contains) {
			t.Errorf("%s output=%s", tc.name, out)
		}
	}
	if out := readNumberedFilter(strings.Join(numbered, "\n")); !strings.Contains(out, "file continues") {
		t.Errorf("read-numbered=%s", out)
	}
	detectNumbered := make([]string, 260)
	for i := range detectNumbered {
		detectNumbered[i] = fmt.Sprintf("%d|", i%9)
	}
	if detected := autoDetectFilter(strings.Join(detectNumbered, "\n")); detected == nil || detected.name != "read-numbered" {
		t.Errorf("read-numbered detected %#v", detected)
	}
	if out := smartTruncateFilter(strings.Join(plain, "\n")); !strings.Contains(out, "lines truncated") {
		t.Errorf("smart-truncate=%s", out)
	}
}

func TestRuntimeIsolationShutdownAndReconstruction(t *testing.T) {
	cfg := fakeTAREConfig(t)
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": marked("ok")}}})
	one, two := NewRuntime(), NewRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := one.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, s := one.Apply(context.Background(), raw, cfg, false); s.Reason != "shutdown" {
		t.Fatalf("closed=%+v", s)
	}
	if _, s := two.Apply(context.Background(), raw, cfg, false); !s.Applied {
		t.Fatalf("other=%+v", s)
	}
	three := NewRuntime()
	if _, s := three.Apply(context.Background(), raw, cfg, false); !s.Applied {
		t.Fatalf("reconstructed=%+v", s)
	}
}

func TestSanitizeStatsSchema(t *testing.T) {
	valid := Stats{Engine: "rtk", Reason: "applied", Applied: true, Selected: 2, Compressed: 1, BytesBefore: 10, BytesAfter: 5, CacheHits: 1, ElapsedMS: 3, Version: "1.2.3", ManifestID: "tare_cli-1"}
	if got := SanitizeStats(valid); got != valid {
		t.Fatalf("valid=%+v", got)
	}
	invalid := SanitizeStats(Stats{Engine: "credential", Reason: "control\n", Selected: -1, Compressed: -2, BytesBefore: -3, BytesAfter: -4, CacheHits: -5, ElapsedMS: -6, Version: "Bearer token", ManifestID: "../secret"})
	if invalid.Engine != "" || invalid.Reason != "" || invalid.Selected != 0 || invalid.Compressed != 0 || invalid.BytesBefore != 0 || invalid.BytesAfter != 0 || invalid.CacheHits != 0 || invalid.ElapsedMS != 0 || invalid.Version != "" || invalid.ManifestID != "" {
		t.Fatalf("invalid=%+v", invalid)
	}
}

func TestTAREAtomicSuccessCacheIdentityAndFailures(t *testing.T) {
	defaultTARE.reset()
	t.Cleanup(defaultTARE.reset)
	cfg := fakeTAREConfig(t)
	count := t.TempDir() + "/count"
	t.Setenv("FAKE_TARE_COUNT_FILE", count)
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": marked("abcd")}, map[string]any{"role": "tool", "content": marked("1234")}}})
	out, stats := Apply(context.Background(), raw, cfg, false)
	if !stats.Applied || stats.Compressed != 2 || stats.Version != "0.2.0" {
		t.Fatalf("stats=%+v out=%s", stats, out)
	}
	out2, stats2 := Apply(context.Background(), raw, cfg, false)
	if !stats2.Applied || stats2.CacheHits != 2 || !bytes.Equal(out, out2) {
		t.Fatalf("cache stats=%+v", stats2)
	}
	counts, _ := os.ReadFile(count)
	if strings.Count(string(counts), "--version") != 1 {
		t.Fatalf("identity calls: %s", counts)
	}
	for _, mode := range []string{"nonzero", "timeout", "stdout-cap", "stderr-cap", "invalid-utf8", "same"} {
		defaultTARE.reset()
		t.Setenv("FAKE_TARE_MODE", mode)
		got, s := Apply(context.Background(), raw, cfg, false)
		if !bytes.Equal(got, raw) || s.Applied {
			t.Fatalf("mode=%s stats=%+v", mode, s)
		}
	}
}

func TestTAREChecksumVersionAbortQueueAndCacheLimits(t *testing.T) {
	defaultTARE.reset()
	t.Cleanup(defaultTARE.reset)
	cfg := fakeTAREConfig(t)
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": marked("abcd")}}})
	bad := cfg
	bad.TARE.SHA256 = strings.Repeat("0", 64)
	_, s := Apply(context.Background(), raw, bad, false)
	if s.Reason != "checksum_mismatch" {
		t.Fatalf("checksum=%+v", s)
	}
	defaultTARE.reset()
	required := cfg
	required.TARE.SHA256 = ""
	_, s = Apply(context.Background(), raw, required, false)
	if s.Reason != "checksum_required" {
		t.Fatalf("checksum required=%+v", s)
	}
	defaultTARE.reset()
	missing := cfg
	missing.TARE.BinaryPath = t.TempDir() + "/missing"
	_, s = Apply(context.Background(), raw, missing, false)
	if s.Reason != "unavailable" {
		t.Fatalf("missing=%+v", s)
	}
	defaultTARE.reset()
	t.Setenv("FAKE_TARE_VERSION", "9.9.9")
	_, s = Apply(context.Background(), raw, cfg, false)
	if s.Reason != "version_mismatch" {
		t.Fatalf("version=%+v", s)
	}
	t.Setenv("FAKE_TARE_VERSION", "")
	defaultTARE.reset()
	t.Setenv("FAKE_TARE_MODE", "")
	warmRaw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": marked("warm")}}})
	if _, warm := Apply(context.Background(), warmRaw, cfg, false); !warm.Applied {
		t.Fatalf("warm identity: %+v", warm)
	}
	t.Setenv("FAKE_TARE_MODE", "timeout")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Stats, 1)
	go func() { _, v := Apply(ctx, raw, cfg, false); done <- v }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	if got := <-done; got.Reason != "aborted" {
		t.Fatalf("abort=%+v", got)
	}
	_, _, children, _, _ := defaultTARE.stats()
	if children != 0 {
		t.Fatalf("children=%d", children)
	}
	defaultTARE.reset()
	t.Setenv("FAKE_TARE_MODE", "")
	if _, warm := Apply(context.Background(), warmRaw, cfg, false); !warm.Applied {
		t.Fatalf("warm queue identity: %+v", warm)
	}
	t.Setenv("FAKE_TARE_MODE", "timeout")
	cfg.TARE.ProcessTimeoutMS = 500
	first := make(chan Stats, 1)
	go func() { _, v := Apply(context.Background(), raw, cfg, false); first <- v }()
	time.Sleep(30 * time.Millisecond)
	_, queued := func() (int, int) { a, q, _, _, _ := defaultTARE.stats(); return a, q }()
	_ = queued
	_, second := Apply(context.Background(), raw, cfg, false)
	if second.Reason != "queue_timeout" {
		t.Fatalf("queue=%+v", second)
	}
	<-first
	t.Setenv("FAKE_TARE_MODE", "")
	defaultTARE.reset()
	cfg.TARE.CacheEntries = 4
	cfg.TARE.CacheBytes = 100
	for i := 0; i < 8; i++ {
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": marked(fmt.Sprintf("value-%d", i))}}})
		Apply(context.Background(), body, cfg, false)
	}
	_, _, _, entries, cacheBytes := defaultTARE.stats()
	if entries > 4 || cacheBytes > 100 {
		t.Fatalf("cache=%d/%d", entries, cacheBytes)
	}
	cfg.TARE.CacheEntries = 1
	cfg.TARE.CacheBytes = 4
	defaultTARE.enforceCacheLimits(cfg.TARE)
	_, _, _, entries, cacheBytes = defaultTARE.stats()
	if entries > 1 || cacheBytes > 4 {
		t.Fatalf("shrunk cache=%d/%d", entries, cacheBytes)
	}
}

func TestTAREInputCapIdentityChangeAndCacheKeysAreOpaque(t *testing.T) {
	e := newTAREEngine()
	cfg := fakeTAREConfig(t)
	if _, reason := e.run(context.Background(), cfg.TARE, []string{"compress"}, []byte("xx"), runLimits{timeoutMS: 100, input: 1, stdout: 10, stderr: 10}); reason != "input_limit" {
		t.Fatalf("reason=%q", reason)
	}
	key := cacheKey(tareSchemaVersion, "secret raw tool output")
	if strings.Contains(key, "secret") {
		t.Fatalf("raw key leaked: %s", key)
	}
	first := e.identity(context.Background(), cfg.TARE)
	if !first.available {
		t.Fatalf("first=%+v", first)
	}
	changed := cfg.TARE
	changed.ManifestID = "tare-cli-test-next"
	second := e.identity(context.Background(), changed)
	if !second.available || second.manifestID == first.manifestID {
		t.Fatalf("identity did not change: %+v %+v", first, second)
	}
}

func TestTARECanceledFirstWaiterDoesNotPoisonIdentity(t *testing.T) {
	e := newTAREEngine()
	cfg := fakeTAREConfig(t)
	count := t.TempDir() + "/count"
	t.Setenv("FAKE_TARE_COUNT_FILE", count)
	t.Setenv("FAKE_TARE_VERSION_DELAY", "1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan tareIdentity, 1)
	go func() { done <- e.identity(ctx, cfg.TARE) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if got := <-done; got.reason != "aborted" {
		t.Fatalf("canceled=%+v", got)
	}
	var wg sync.WaitGroup
	results := make(chan tareIdentity, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- e.identity(context.Background(), cfg.TARE) }()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if !got.available {
			t.Fatalf("later=%+v", got)
		}
	}
	calls, _ := os.ReadFile(count)
	if strings.Count(string(calls), "--version") != 1 {
		t.Fatalf("version calls=%s", calls)
	}
}

func TestTARECapKillsBlockingChildPromptlyAndShutdownRejectsQueue(t *testing.T) {
	e := newTAREEngine()
	cfg := fakeTAREConfig(t)
	for _, mode := range []string{"stdout-cap-block", "stderr-cap-block"} {
		t.Setenv("FAKE_TARE_MODE", mode)
		start := time.Now()
		_, reason := e.run(context.Background(), cfg.TARE, []string{"compress"}, []byte(`[{"text":"x"}]`), runLimits{timeoutMS: 3000, input: 1024, stdout: 1024, stderr: 1024})
		if !strings.HasSuffix(reason, "_limit") || time.Since(start) > time.Second {
			t.Fatalf("mode=%s reason=%s elapsed=%s", mode, reason, time.Since(start))
		}
		_, _, children, _, _ := e.stats()
		if children != 0 {
			t.Fatalf("children=%d", children)
		}
	}
	t.Setenv("FAKE_TARE_MODE", "timeout")
	first := make(chan string, 1)
	second := make(chan string, 1)
	go func() {
		_, r := e.run(context.Background(), cfg.TARE, []string{"compress"}, []byte("x"), runLimits{timeoutMS: 5000, input: 10, stdout: 10, stderr: 10})
		first <- r
	}()
	time.Sleep(30 * time.Millisecond)
	go func() {
		_, r := e.run(context.Background(), cfg.TARE, []string{"compress"}, []byte("x"), runLimits{timeoutMS: 5000, input: 10, stdout: 10, stderr: 10})
		second <- r
	}()
	time.Sleep(30 * time.Millisecond)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	a, b := <-first, <-second
	if a != "shutdown" {
		t.Fatalf("active=%s", a)
	}
	if b != "shutdown" {
		t.Fatalf("queued=%s", b)
	}
}

func TestShutdownBarrierRejectsIdentityRunAfterAdmission(t *testing.T) {
	e := newTAREEngine()
	entered := make(chan struct{})
	release := make(chan struct{})
	e.identityBarrier = func() { close(entered); <-release }
	var versionCalls atomic.Int32
	e.readIdentityBinary = func(string) ([]byte, error) { return []byte("fixture"), nil }
	e.runIdentityVersion = func(context.Context, config.TAREStructuralConfig) ([]byte, string) {
		versionCalls.Add(1)
		return []byte("tare 0.2.0"), ""
	}
	sum := sha256.Sum256([]byte("fixture"))
	cfg := config.ContextCompressionConfig{Engine: config.ContextCompressionTARE, RawCapBytes: 1024, TARE: config.TAREStructuralConfig{BinaryPath: "fixture", SHA256: fmt.Sprintf("%x", sum), AllowedVersions: []string{"0.2.0"}, ManifestID: "fixture", ProcessTimeoutMS: 100, QueueTimeoutMS: 100, InputLimitBytes: 2048, StdoutLimitBytes: 1024, StderrLimitBytes: 1024, GlobalConcurrency: 1, CacheEntries: 2, CacheBytes: 1024}}
	runtime := &Runtime{tare: e}
	raw := []byte(`{"messages":[{"role":"tool","content":"eligible"}]}`)
	result := make(chan Stats, 1)
	go func() { _, stats := runtime.Apply(context.Background(), raw, cfg, false); result <- stats }()
	<-entered
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownDone <- runtime.Shutdown(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before admitted identity drained: %v", err)
	default:
	}
	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if stats := <-result; stats.Reason != "shutdown" {
		t.Fatalf("result=%+v", stats)
	}
	if versionCalls.Load() != 0 {
		t.Fatalf("version calls=%d", versionCalls.Load())
	}
	if _, stats := runtime.Apply(context.Background(), raw, cfg, false); stats.Reason != "shutdown" {
		t.Fatalf("late=%+v", stats)
	}
}

func TestShutdownDuringActiveIdentityVersionReportsShutdown(t *testing.T) {
	e := newTAREEngine()
	started := make(chan struct{})
	e.readIdentityBinary = func(string) ([]byte, error) { return []byte("fixture"), nil }
	e.runIdentityVersion = func(context.Context, config.TAREStructuralConfig) ([]byte, string) {
		close(started)
		<-e.shutdownCh
		return nil, "shutdown"
	}
	sum := sha256.Sum256([]byte("fixture"))
	cfg := config.ContextCompressionConfig{Engine: config.ContextCompressionTARE, RawCapBytes: 1024, TARE: config.TAREStructuralConfig{BinaryPath: "fixture", SHA256: fmt.Sprintf("%x", sum), AllowedVersions: []string{"0.2.0"}, ManifestID: "fixture", ProcessTimeoutMS: 100, QueueTimeoutMS: 100, InputLimitBytes: 2048, StdoutLimitBytes: 1024, StderrLimitBytes: 1024, GlobalConcurrency: 1, CacheEntries: 2, CacheBytes: 1024}}
	runtime := &Runtime{tare: e}
	raw := []byte(`{"messages":[{"role":"tool","content":"eligible"}]}`)
	result := make(chan Stats, 1)
	go func() { _, stats := runtime.Apply(context.Background(), raw, cfg, false); result <- stats }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := <-result; stats.Reason != "shutdown" {
		t.Fatalf("result=%+v", stats)
	}
}

func TestConcurrentDuplicateCacheReplacementAccounting(t *testing.T) {
	e := newTAREEngine()
	cfg := fakeTAREConfig(t).TARE
	entry := cacheEntry{key: cacheKey("secret raw"), value: "ok", bytes: 2}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.put(entry, cfg) }()
	}
	wg.Wait()
	_, _, _, entries, size := e.stats()
	if entries != 1 || size != 2 {
		t.Fatalf("cache=%d/%d", entries, size)
	}
	if strings.Contains(entry.key, "secret") {
		t.Fatal("raw key leaked")
	}
}

func TestTARESkipsOversizedSlotAndDoesNotUseRTKMinimum(t *testing.T) {
	defaultTARE.reset()
	t.Cleanup(defaultTARE.reset)
	cfg := fakeTAREConfig(t)
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": strings.Repeat("x", maxTARESlotBytes+1)}, map[string]any{"role": "tool", "content": marked("ok")}}})
	out, s := Apply(context.Background(), raw, cfg, false)
	if !s.Applied || s.Selected != 1 {
		t.Fatalf("stats=%+v", s)
	}
	if !bytes.Contains(out, []byte(strings.Repeat("x", 100))) {
		t.Fatal("oversized slot not preserved")
	}
}
