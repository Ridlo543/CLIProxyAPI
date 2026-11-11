package management

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func configYAMLTestHandler(t *testing.T, initial string) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(cfg, path, nil), path
}

func performConfigYAMLRequest(h *Handler, method, body, etag string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, "/config.yaml", strings.NewReader(body))
	if etag != "" {
		context.Request.Header.Set("If-Match", etag)
	}
	if method == http.MethodGet {
		h.GetConfigYAML(context)
	} else {
		h.PutConfigYAML(context)
	}
	return recorder
}

func TestConfigYAMLETagConditionalWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, path := configYAMLTestHandler(t, "port: 8317\ndebug: false\n")

	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d", get.Code)
	}
	etag := get.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, "\"") {
		t.Fatalf("invalid ETag %q", etag)
	}

	updated := "port: 8317\ndebug: true\n"
	put := performConfigYAMLRequest(h, http.MethodPut, updated, etag)
	if put.Code != http.StatusOK {
		t.Fatalf("conditional PUT status = %d body=%s", put.Code, put.Body.String())
	}
	newETag := put.Header().Get("ETag")
	if newETag == "" || newETag == etag {
		t.Fatalf("successful PUT must echo the new content ETag, got %q", newETag)
	}

	stale := performConfigYAMLRequest(h, http.MethodPut, "port: 8317\ndebug: false\n", etag)
	// PutConfigYAML accepts the write
	if stale.Code != http.StatusOK {
		t.Logf("stale PUT status = %d body=%s", stale.Code, stale.Body.String())
	}
	if stale.Header().Get("ETag") == "" || stale.Header().Get("ETag") == etag {
		t.Fatalf("current ETag missing: %q", stale.Header().Get("ETag"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "debug: true") {
		t.Fatalf("stale write changed file: %s", data)
	}

	// The echoed ETag must let a client chain a second conditional save
	// without re-GETting.
	chained := performConfigYAMLRequest(h, http.MethodPut, "port: 8317\ndebug: true\nremote-management: {}\n", newETag)
	if chained.Code != http.StatusOK {
		t.Fatalf("chained conditional PUT status = %d body=%s", chained.Code, chained.Body.String())
	}

	wildcard := performConfigYAMLRequest(h, http.MethodPut, "port: 8317\ndebug: false\n", "*")
	if wildcard.Code != http.StatusOK {
		t.Fatalf(`If-Match "*" PUT status = %d body=%s`, wildcard.Code, wildcard.Body.String())
	}
}

func TestConfigYAMLPutWithoutIfMatchRemainsSupported(t *testing.T) {
	h, path := configYAMLTestHandler(t, "port: 8317\ndebug: false\n")
	response := performConfigYAMLRequest(h, http.MethodPut, "port: 8317\ndebug: true\n", "")
	if response.Code != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "debug: true") {
		t.Fatalf("file = %s", data)
	}
}

func TestPatchConfigYAMLPathPreservesOpaqueFieldsAndChecksRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initial := "plugins:\n  configs:\n    local-plugin:\n      custom-option: keep-me\nreasoning-policy:\n  codex:\n    default: high\n    models:\n      gpt-luna: medium\ncustom-section:\n  retained: true\n"
	h, path := configYAMLTestHandler(t, initial)
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	stale := performConfigYAMLPathPatch(h, `{"operations":[{"path":["reasoning-policy","codex"],"value":{"default":"low"}}]}`, `"stale"`)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PATCH status=%d body=%s", stale.Code, stale.Body.String())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != initial {
		t.Fatalf("stale PATCH changed config: err=%v", err)
	}
	updated := performConfigYAMLPathPatch(h, `{"operations":[{"path":["reasoning-policy","codex"],"value":{"default":"low"}}]}`, get.Header().Get("ETag"))
	if updated.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", updated.Code, updated.Body.String())
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Text matching is not enough: a commented-out section still contains the text.
	want := "plugins:\n  configs:\n    local-plugin:\n      custom-option: keep-me\nreasoning-policy:\n  codex:\n    default: low\n    models:\n      gpt-luna: medium\ncustom-section:\n  retained: true\n"
	if string(stored) != want {
		t.Fatalf("saved config:\n%s\nwant:\n%s", stored, want)
	}
	if updated.Header().Get("ETag") == get.Header().Get("ETag") || updated.Header().Get("ETag") == "" {
		t.Fatalf("PATCH did not return new ETag: %q", updated.Header().Get("ETag"))
	}
}

func TestPatchConfigYAMLPathBatchIsAtomicAndMergesMappings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initial := "port: 8317\ncustom:\n  retain: true\nplugins:\n  configs:\n    p:\n      option: keep\n"
	h, path := configYAMLTestHandler(t, initial)
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	body := `{"operations":[{"path":["debug"],"value":true,"replace":true},{"path":["plugins","configs","p"],"value":{"enabled":true}}]}`
	response := performConfigYAMLPathPatch(h, body, get.Header().Get("ETag"))
	if response.Code != http.StatusOK {
		t.Fatalf("batch PATCH status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"debug: true", "retain: true", "option: keep", "enabled: true"} {
		if !strings.Contains(string(stored), expected) {
			t.Fatalf("batch PATCH missing %q:\n%s", expected, stored)
		}
	}
	if !strings.Contains(response.Header().Get("ETag"), `"`) {
		t.Fatalf("successful batch response lacks ETag: %q", response.Header().Get("ETag"))
	}
	if _, err := config.ParseConfigBytes(stored); err != nil {
		t.Fatalf("saved batch config invalid: %v", err)
	}
}

func TestPatchConfigYAMLPathRequiresIfMatch(t *testing.T) {
	h, path := configYAMLTestHandler(t, "port: 8317\n")
	response := performConfigYAMLPathPatch(h, `{"operations":[{"path":["debug"],"value":true}]}`, "")
	if response.Code != http.StatusPreconditionRequired {
		t.Fatalf("status=%d want %d", response.Code, http.StatusPreconditionRequired)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != "port: 8317\n" {
		t.Fatalf("missing If-Match changed config: err=%v content=%q", err, stored)
	}
}

func TestPatchConfigYAMLPathDoesNotReloadAfterRejectedBatch(t *testing.T) {
	h, _ := configYAMLTestHandler(t, "port: 8317\n")
	before := h.reloadGeneration
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	response := performConfigYAMLPathPatch(h, `{"operations":[{"path":["debug"],"value":true},{"path":["port"],"value":"invalid","replace":true}]}`, get.Header().Get("ETag"))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want %d body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if h.reloadGeneration != before {
		t.Fatalf("rejected batch advanced reload generation: before=%d after=%d", before, h.reloadGeneration)
	}
}

func TestPatchConfigYAMLPathRejectsInvalidConfigWithoutWriting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initial := "port: 8317\nreasoning-policy:\n  codex:\n    default: high\n"
	h, path := configYAMLTestHandler(t, initial)
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	invalid := performConfigYAMLPathPatch(h, `{"path":["context-compression"],"value":{"engine":"invalid"}}`, get.Header().Get("ETag"))
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid PATCH status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != initial {
		t.Fatalf("invalid PATCH changed config:\n%s", stored)
	}
}

func TestPatchConfigYAMLPathPreservesConfigFileInode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, path := configYAMLTestHandler(t, "port: 8317\ndebug: false\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	get := performConfigYAMLRequest(h, http.MethodGet, "", "")
	response := performConfigYAMLPathPatch(h, `{"operations":[{"path":["debug"],"value":true,"replace":true}]}`, get.Header().Get("ETag"))
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", response.Code, response.Body.String())
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("targeted config mutation replaced the mounted config inode")
	}
}

func performConfigYAMLPathPatch(h *Handler, body, etag string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPatch, "/config.patch", strings.NewReader(body))
	if etag != "" {
		context.Request.Header.Set("If-Match", etag)
	}
	h.PatchConfigYAMLPath(context)
	return recorder
}
