package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

// The executor receives the UPSTREAM model name, not the client-facing alias:
// sdk/cliproxy/auth/conductor_execution.go sets execReq.Model = upstreamModel
// after resolving the alias table. A persona keyed on the alias name therefore
// never fires, because the executor never sees that string.
//
// This test documents that constraint so the config is written correctly:
// system-instruction-models must list the UPSTREAM model name.
func TestInjectSystemInstruction_ExecutorSeesUpstreamName(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash"}, // upstream name
	}

	// What the executor actually receives after alias resolution.
	executorModelName := "gemini-3.8-flash"

	got := InjectSystemInstruction(executorModelName, []byte(`{"request":{"contents":[]}}`), cfg)
	if gjson.GetBytes(got, "request.systemInstruction").Exists() {
		t.Log("persona fires when config lists the upstream model name")
	} else {
		t.Fatalf("persona must fire for the upstream model name, got %s", got)
	}
}
