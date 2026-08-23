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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PUT status = %d", stale.Code)
	}
	if !strings.Contains(stale.Body.String(), configRevisionMismatchCode) {
		t.Fatalf("stale body = %s", stale.Body.String())
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
