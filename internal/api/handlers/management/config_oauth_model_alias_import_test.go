package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func aliasHandler(t *testing.T, initial string) *Handler {
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

func performAlias(h *Handler, method, channel string, body any) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	raw, _ := json.Marshal(body)
	ctx.Request = httptest.NewRequest(method, "/oauth-model-alias/"+channel, bytes.NewReader(raw))
	ctx.Params = gin.Params{{Key: "channel", Value: channel}}
	if method == http.MethodPost {
		h.AddOAuthModelAliases(ctx)
	} else {
		h.GetOAuthModelAliasChannel(ctx)
	}
	return recorder
}

func TestAddOAuthModelAliasesRoundtripAndReload(t *testing.T) {
	h := aliasHandler(t, "port: 8317\n")
	body := map[string]any{"aliases": []map[string]any{
		{"name": "gemini-2.5-pro", "alias": "pro", "display-name": "Pro"},
		{"name": "gemini-2.5-flash", "alias": "flash"},
	}}
	resp := performAlias(h, http.MethodPost, "antigravity", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Added   int `json:"added"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil || payload.Added != 2 {
		t.Fatalf("payload=%s err=%v", resp.Body.String(), err)
	}
	list := performAlias(h, http.MethodGet, "antigravity", nil)
	if !strings.Contains(list.Body.String(), "\"alias\":\"pro\"") {
		t.Fatalf("list=%s", list.Body.String())
	}
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, errLoad := config.LoadConfig(h.configFilePath)
	if errLoad != nil {
		t.Fatalf("reload: %v", errLoad)
	}
	survived := reloaded.OAuthModelAlias["antigravity"]
	if len(survived) != 2 || survived[0].Name != "gemini-2.5-pro" || survived[0].Alias != "pro" || survived[0].DisplayName != "Pro" || strings.Contains(string(data), "\"display-name\"") == false && !strings.Contains(string(data), "display-name: Pro") {
		t.Fatalf("persisted=%s entries=%+v", data, survived)
	}
}

func TestAddOAuthModelAliasesRejectsDuplicatesAndUnknownChannel(t *testing.T) {
	h := aliasHandler(t, "oauth-model-alias:\n  claude:\n    - name: up\n      alias: taken\n")
	dup := performAlias(h, http.MethodPost, "claude", map[string]any{
		"aliases": []map[string]any{{"name": "other", "alias": "Taken"}},
	})
	if dup.Code != http.StatusConflict || !strings.Contains(dup.Body.String(), "duplicate_alias") || !strings.Contains(strings.ToLower(dup.Body.String()), "taken") {
		t.Fatalf("dup=%d %s", dup.Code, dup.Body.String())
	}
	unknown := performAlias(h, http.MethodPost, "not-a-channel", map[string]any{
		"aliases": []map[string]any{{"name": "a", "alias": "b"}},
	})
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown oauth channel") {
		t.Fatalf("unknown=%d %s", unknown.Code, unknown.Body.String())
	}
	data, _ := os.ReadFile(h.configFilePath)
	if !strings.Contains(string(data), "alias: taken") {
		t.Fatalf("config mutated on rejection: %q", string(data))
	}
}
