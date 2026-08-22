package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
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
