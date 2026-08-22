package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

func TestClaudeModelsIncludesConfiguredModelGroup(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "claude", Model: "claude-test"}}}}}, nil)
	models := NewClaudeCodeAPIHandler(base).Models()
	for _, model := range models {
		if model["id"] == "code-auto" {
			if model["type"] != "model" || model["owned_by"] != "ainyrouter/model-group" {
				t.Fatalf("group model = %#v", model)
			}
			return
		}
	}
	t.Fatal("configured model group missing from Claude listing")
}
