package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestRewriteModelFieldSetsMemberModel(t *testing.T) {
	out, err := rewriteModelField([]byte(`{"model":"leader","messages":[{"role":"user","content":"hi"}]}`), "m-shared")
	if err != nil {
		t.Fatalf("rewriteModelField: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(top["model"]) != `"m-shared"` {
		t.Fatalf("model = %s, want \"m-shared\"", top["model"])
	}
	var msgs []map[string]string
	if err := json.Unmarshal(top["messages"], &msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("messages lost: %v %v", msgs, err)
	}
}

func TestCombosAugmentModelsWritesSingleJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{Name: "c1", Strategy: config.ComboStrategyFallback, Models: []config.ComboModelRef{{Provider: "p", Model: "m"}}},
	}})
	defer combos.SyncFromConfig(&config.Config{})

	r := gin.New()
	s := &Server{}
	r.GET("/v1/models", s.combosAugmentModels(func(c *gin.Context) {
		c.JSON(200, gin.H{"object": "list", "data": []gin.H{{"id": "real-1"}}})
	}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))

	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body := w.Body.String()
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not a single JSON document: %v; body=%q", err, body)
	}
	if len(parsed.Data) != 2 || parsed.Data[0].ID != "real-1" || parsed.Data[1].ID != "c1" {
		t.Fatalf("unexpected data: %+v", parsed.Data)
	}
}
