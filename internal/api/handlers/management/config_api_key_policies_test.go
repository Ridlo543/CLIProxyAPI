package management

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func policiesTestHandler(t *testing.T, initial string) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(cfg, path, nil)
}

func performJSON(h *Handler, method, path string, body any) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = strings.NewReader("")
	}
	ctx.Request = httptest.NewRequest(method, path, reader)
	switch {
	case strings.HasSuffix(path, "/api-key-policies") && method == http.MethodGet:
		h.GetAPIKeyPolicies(ctx)
	case strings.HasSuffix(path, "/api-key-policies") && method == http.MethodPut:
		h.PutAPIKeyPolicies(ctx)
	case strings.Contains(path, "import-models"):
		ctx.Params = gin.Params{{Key: "name", Value: "vendor"}}
		h.ImportOpenAICompatModels(ctx)
	}
	return recorder
}

func TestAPIKeyPoliciesCRUDRoundtrip(t *testing.T) {
	h := policiesTestHandler(t, "port: 8317\napi-keys:\n  - sk-abcdefghij\n")
	put := performJSON(h, http.MethodPut, "/api-key-policies", []map[string]any{{
		"key":       "sk-abcdefghij",
		"models":    []string{"gpt-a"},
		"providers": []string{"codex"},
		"token-limit": map[string]any{
			"window": "daily",
			"limit":  100,
		},
	}})
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	get := performJSON(h, http.MethodGet, "/api-key-policies", nil)
	var payload struct {
		Policies []map[string]any `json:"api-key-policies"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Policies) != 1 || !strings.Contains(payload.Policies[0]["key"].(string), "hij") || strings.Contains(payload.Policies[0]["key"].(string), "sk-abcd") {
		t.Fatalf("masked list=%s", get.Body.String())
	}
	snapshot := apikeypolicy.Default().UsageSnapshot() // enforcer updated without error
	_ = snapshot
}

func TestImportModelsMergesAndSkips(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "new-model"},
			{"id": "existing"},
			{"id": "existing-alias"},
		}})
	}))
	defer upstream.Close()

	h := policiesTestHandler(t, "openai-compatibility:\n  - name: vendor\n    base-url: \""+upstream.URL+"\"\n    models:\n      - name: existing\n        alias: existing-alias\n    api-key-entries:\n      - api-key: secret\n")

	resp := performJSON(h, http.MethodPost, "/openai-compatibility/vendor/import-models", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth header=%q", gotAuth)
	}
	var payload struct {
		Added      []string `json:"added"`
		Skipped    []string `json:"skipped"`
		AddedCount int      `json:"added_count"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &payload)
	if payload.AddedCount != 1 || len(payload.Added) != 1 || payload.Added[0] != "new-model" || len(payload.Skipped) != 2 {
		t.Fatalf("payload=%s", resp.Body.String())
	}
}

func TestImportModelsUpstreamFailureLeavesConfigUntouched(t *testing.T) {
	failCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failCount++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	initial := "openai-compatibility:\n  - name: vendor\n    base-url: \"" + upstream.URL + "\"\n"
	h := policiesTestHandler(t, initial)

	resp := performJSON(h, http.MethodPost, "/openai-compatibility/vendor/import-models", nil)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "base-url: \""+upstream.URL+"\"") || strings.Contains(string(data), "models:") {
		t.Fatalf("config mutated on failure: %s", data)
	}
	if failCount == 0 {
		t.Fatal("upstream was not called")
	}
}
