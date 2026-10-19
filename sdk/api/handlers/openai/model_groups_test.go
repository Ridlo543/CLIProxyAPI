package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestOpenAIModelsIncludesConfiguredModelGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "codex", Model: "gpt-test"}}}}}, nil)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", h.OpenAIModels)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/v1/models", nil))
	body := recorder.Body.String()
	if recorder.Code != 200 || !strings.Contains(body, `"id":"code-auto"`) || !strings.Contains(body, `"owned_by":"ainyrouter/model-group"`) || !strings.Contains(body, `"type":"model-group"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
}

func TestCodexClientModelsDeliberatelyOmitsModelGroups(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "codex", Model: "gpt-test"}}}}}, nil)
	response := NewOpenAIAPIHandler(base).codexClientModelsResponse()
	models, _ := response["models"].([]map[string]any)
	for _, model := range models {
		if model["slug"] == "code-auto" {
			t.Fatalf("Codex client catalog unexpectedly contains model group: %#v", model)
		}
	}
}

func TestOpenAIModelsIncludesContextLengthAndLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := registry.GetGlobalRegistry()
	clientID := "test-context-length-limits"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{
		{
			ID:            "deepseek-v4.1-flash-1017",
			Object:        "model",
			OwnedBy:       "jerouter",
			ContextLength: 524288,
		},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", h.OpenAIModels)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/v1/models", nil))
	body := recorder.Body.String()
	if recorder.Code != 200 {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	if !strings.Contains(body, `"context_length":524288`) || !strings.Contains(body, `"context_window":524288`) {
		t.Fatalf("expected context_length and context_window 524288 in body: %s", body)
	}
}
