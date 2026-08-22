package gemini

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

func TestGeminiModelsIncludesConfiguredModelGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "gemini", Model: "gemini-test"}}}}}, nil)
	h := NewGeminiAPIHandler(base)
	router := gin.New()
	router.GET("/v1beta/models", h.GeminiModels)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/v1beta/models", nil))
	body := recorder.Body.String()
	if recorder.Code != 200 || !strings.Contains(body, `"name":"models/code-auto"`) || !strings.Contains(body, `"description":"ainyrouter/model-group"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
}
