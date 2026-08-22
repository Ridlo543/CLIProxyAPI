package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type oauthAliasThinkingExecutor struct{ level chan string }

func (e *oauthAliasThinkingExecutor) Identifier() string { return "claude" }
func (e *oauthAliasThinkingExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	info, ok := ResolvedAPIKeyModelInfo(req)
	if !ok {
		return cliproxyexecutor.Response{}, fmt.Errorf("exact OAuth alias capability was not bound")
	}
	if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		return cliproxyexecutor.Response{}, fmt.Errorf("bound capability has no levels")
	}
	e.level <- info.Thinking.Levels[0]
	return cliproxyexecutor.Response{Payload: req.Payload}, nil
}
func (*oauthAliasThinkingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (*oauthAliasThinkingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*oauthAliasThinkingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (*oauthAliasThinkingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestExecuteOAuthAliasThinkingUsesGlobalAndSelectedPerAuthCapability(t *testing.T) {
	tests := []struct {
		name            string
		global, perAuth []string
		want            string
	}{
		{name: "global", global: []string{"high"}, want: "high"},
		{name: "per-auth overrides global", global: []string{"high"}, perAuth: []string{"max"}, want: "max"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{OAuthModelAlias: map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "claude-upstream", Alias: "public-model", Thinking: &registry.ThinkingSupport{Levels: tc.global}}}}})
			manager.SetOAuthModelAlias(manager.runtimeConfigSnapshot().OAuthModelAlias)
			executor := &oauthAliasThinkingExecutor{level: make(chan string, 1)}
			manager.RegisterExecutor(executor)
			auth := &Auth{ID: "oauth-" + tc.name, Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
			if len(tc.perAuth) > 0 {
				SetOAuthModelAliasesAttribute(auth, []internalconfig.OAuthModelAlias{{Name: "claude-upstream", Alias: "public-model", Thinking: &registry.ThinkingSupport{Levels: tc.perAuth}}})
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "public-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "public-model(max)", Payload: []byte(`{}`)}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := <-executor.level; got != tc.want {
				t.Fatalf("attempt capability level = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecuteOAuthAliasThinkingPerAuthBaseFallbackBeatsGlobalExactSuffix(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{OAuthModelAlias: map[string][]internalconfig.OAuthModelAlias{"claude": {{
		Name: "global-upstream", Alias: "public(low)", Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
	}}}})
	manager.SetOAuthModelAlias(manager.runtimeConfigSnapshot().OAuthModelAlias)
	executor := &oauthAliasThinkingExecutor{level: make(chan string, 1)}
	manager.RegisterExecutor(executor)
	auth := &Auth{ID: "oauth-conflict", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	SetOAuthModelAliasesAttribute(auth, []internalconfig.OAuthModelAlias{{
		Name: "selected-upstream", Alias: "public", Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
	}})
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "public"}, {ID: "public(low)"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "public(low)", Payload: []byte(`{}`)}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := <-executor.level; got != "high" {
		t.Fatalf("attempt capability level = %q, want selected per-auth high", got)
	}
}
