package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type oauthAliasRefreshCapabilityExecutor struct {
	mu               sync.Mutex
	stream           bool
	refreshedAliases []internalconfig.OAuthModelAlias
	calls            int
	secondModel      string
	secondLevels     []string
}

func (e *oauthAliasRefreshCapabilityExecutor) Identifier() string { return "claude" }
func (e *oauthAliasRefreshCapabilityExecutor) capture(req cliproxyexecutor.Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.calls == 1 {
		return &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"}
	}
	e.secondModel = req.Model
	if info, ok := ResolvedAPIKeyModelInfo(req); ok && info.Thinking != nil {
		e.secondLevels = append([]string(nil), info.Thinking.Levels...)
	}
	return nil
}
func (e *oauthAliasRefreshCapabilityExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.capture(req); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"model":"` + req.Model + `"}`)}, nil
}
func (e *oauthAliasRefreshCapabilityExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.capture(req); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"model":"` + req.Model + `"}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func (*oauthAliasRefreshCapabilityExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *oauthAliasRefreshCapabilityExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	refreshed := auth.Clone()
	delete(refreshed.Attributes, oauthModelAliasesAttributeKey)
	SetOAuthModelAliasesAttribute(refreshed, e.refreshedAliases)
	if refreshed.Metadata == nil {
		refreshed.Metadata = make(map[string]any)
	}
	refreshed.Metadata["access_token"] = "fresh"
	return refreshed, nil
}
func (*oauthAliasRefreshCapabilityExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestOAuthAliasRefreshRebuildsNormalAndStreamingAttempts(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		refreshed  []internalconfig.OAuthModelAlias
		wantModel  string
		wantLevels []string
	}{
		{name: "normal changes override", refreshed: []internalconfig.OAuthModelAlias{{Name: "new-upstream", Alias: "public", Thinking: &registry.ThinkingSupport{Levels: []string{"max"}}}}, wantModel: "new-upstream", wantLevels: []string{"max"}},
		{name: "stream removes alias and capability", stream: true, wantModel: "public"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			executor := &oauthAliasRefreshCapabilityExecutor{stream: tc.stream, refreshedAliases: tc.refreshed}
			manager.RegisterExecutor(executor)
			auth := &Auth{ID: "refresh-" + tc.name, Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"access_token": "stale", "refresh_token": "refresh"}}
			SetOAuthModelAliasesAttribute(auth, []internalconfig.OAuthModelAlias{{Name: "old-upstream", Alias: "public", Thinking: &registry.ThinkingSupport{Levels: []string{"high"}}}})
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "public"}, {ID: "old-upstream"}, {ID: "new-upstream"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if tc.stream {
				result, err := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "public", Payload: []byte(`{}`)}, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatalf("ExecuteStream() error = %v", err)
				}
				for range result.Chunks {
				}
			} else if _, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "public", Payload: []byte(`{}`)}, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if executor.secondModel != tc.wantModel {
				t.Fatalf("retry model = %q, want %q", executor.secondModel, tc.wantModel)
			}
			if len(executor.secondLevels) != len(tc.wantLevels) || (len(tc.wantLevels) > 0 && executor.secondLevels[0] != tc.wantLevels[0]) {
				t.Fatalf("retry levels = %#v, want %#v", executor.secondLevels, tc.wantLevels)
			}
		})
	}
}
