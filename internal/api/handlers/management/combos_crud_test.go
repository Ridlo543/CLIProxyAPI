package management

import (
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

func newCombosTestServer(t *testing.T) (*gin.Engine, *Handler) {
	t.Helper()
	dir := t.TempDir()
	// SaveConfigPreserveComments reads the existing file before writing.
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: cfgPath,
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v0/management/combos", h.ListCombos)
	r.GET("/v0/management/combos/:name", h.GetCombo)
	r.POST("/v0/management/combos", h.CreateCombo)
	r.PUT("/v0/management/combos/:name", h.UpdateCombo)
	r.DELETE("/v0/management/combos/:name", h.DeleteCombo)
	return r, h
}

func doJSON(t *testing.T, r *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if len(w.Body.Bytes()) > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s %s: %v body=%s", method, path, err, w.Body.String())
		}
	}
	return w.Code, out
}

const comboBody = `{"name":"leader","strategy":"fallback","models":[
  {"provider":"antigravity","model":"gemini-3-pro"},
  {"provider":"opencode-free","model":"claude-sonnet-4"}]}`

func TestCombosCRUDLifecycle(t *testing.T) {
	r, _ := newCombosTestServer(t)

	// Create → 201
	code, _ := doJSON(t, r, http.MethodPost, "/v0/management/combos", comboBody)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}

	// Duplicate name → 409
	code, out := doJSON(t, r, http.MethodPost, "/v0/management/combos", comboBody)
	if code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d (%v)", code, out)
	}

	// List → exactly one combo
	code, out = doJSON(t, r, http.MethodGet, "/v0/management/combos", "")
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	combos, _ := out["combos"].([]any)
	if len(combos) != 1 {
		t.Fatalf("list len = %d, want 1", len(combos))
	}

	// Update renames + changes strategy
	code, _ = doJSON(t, r, http.MethodPut, "/v0/management/combos/leader",
		`{"name":"leader-v2","strategy":"round-robin","models":[
		  {"provider":"a","model":"m1"},
		  {"provider":"b","model":"m2"}]}`)
	if code != http.StatusOK {
		t.Fatalf("update status = %d", code)
	}

	// Old name gone, new name present
	if code, _ = doJSON(t, r, http.MethodGet, "/v0/management/combos/leader", ""); code != http.StatusNotFound {
		t.Fatalf("old name still resolvable")
	}
	code, out = doJSON(t, r, http.MethodGet, "/v0/management/combos/leader-v2", "")
	if code != http.StatusOK {
		t.Fatalf("get new name status = %d", code)
	}
	if out["strategy"] != "round-robin" {
		t.Fatalf("strategy = %v", out["strategy"])
	}

	// Delete
	if code, _ = doJSON(t, r, http.MethodDelete, "/v0/management/combos/leader-v2", ""); code != http.StatusOK {
		t.Fatalf("delete status = %d", code)
	}
	if code, _ = doJSON(t, r, http.MethodGet, "/v0/management/combos/leader-v2", ""); code != http.StatusNotFound {
		t.Fatalf("deleted combo still present")
	}
}

func TestCombosValidation(t *testing.T) {
	r, _ := newCombosTestServer(t)

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"missing name", `{"strategy":"fallback","models":[{"provider":"a","model":"m"},{"provider":"b","model":"n"}]}`, http.StatusBadRequest},
		{"bad strategy", `{"name":"x","strategy":"random","models":[{"provider":"a","model":"m"},{"provider":"b","model":"n"}]}`, http.StatusBadRequest},
		{"too few models", `{"name":"x","strategy":"fallback","models":[{"provider":"a","model":"m"}]}`, http.StatusBadRequest},
		{"duplicate member", `{"name":"x","strategy":"fallback","models":[{"provider":"a","model":"m"},{"provider":"A","model":"M"}]}`, http.StatusBadRequest},
		{"empty provider", `{"name":"x","strategy":"fallback","models":[{"provider":"","model":"m"},{"provider":"b","model":"n"}]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if code, out := doJSON(t, r, http.MethodPost, "/v0/management/combos", tc.body); code != tc.status {
			t.Fatalf("%s: status = %d want %d (%v)", tc.name, code, tc.status, out)
		}
	}
}

func TestCombosNormalizeStrategyCase(t *testing.T) {
	r, _ := newCombosTestServer(t)
	code, out := doJSON(t, r, http.MethodPost, "/v0/management/combos",
		`{"name":"casing","strategy":"  ROUND-ROBIN ","models":[
		  {"provider":" a ","model":" m1 "},
		  {"provider":"b","model":"m2"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", code, out)
	}
	_, h := newCombosTestServer(t)
	_ = h
	code, out = doJSON(t, r, http.MethodGet, "/v0/management/combos/casing", "")
	if out["strategy"] != string(config.ComboStrategyRoundRobin) {
		t.Fatalf("normalized strategy = %v", out["strategy"])
	}
	models, _ := out["models"].([]any)
	first, _ := models[0].(map[string]any)
	if first["provider"] != "a" || first["model"] != "m1" {
		t.Fatalf("member not trimmed: %v", first)
	}
}
