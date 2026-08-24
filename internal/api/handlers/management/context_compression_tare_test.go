package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTareInfoTestServer() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.GET("/v0/management/context-compression/tare-binary", h.GetTareBinaryInfo)
	return r
}

func TestGetTareBinaryInfoHashesFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tare")
	payload := []byte("#!/usr/bin/env tare\nfake binary payload for hashing\n")
	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatalf("write temp binary: %v", err)
	}
	sum := sha256.Sum256(payload)

	r := newTareInfoTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/context-compression/tare-binary?path="+bin, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
		SHA256    string `json:"sha256"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 = %s, want %s", resp.SHA256, hex.EncodeToString(sum[:]))
	}
	if resp.SizeBytes != int64(len(payload)) {
		t.Fatalf("sizeBytes = %d, want %d", resp.SizeBytes, len(payload))
	}
}

func TestGetTareBinaryInfoRejectsRelativePath(t *testing.T) {
	r := newTareInfoTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/context-compression/tare-binary?path=relative/tare", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestGetTareBinaryInfoReportsMissingFile(t *testing.T) {
	r := newTareInfoTestServer()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	req := httptest.NewRequest(http.MethodGet, "/v0/management/context-compression/tare-binary?path="+missing, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
