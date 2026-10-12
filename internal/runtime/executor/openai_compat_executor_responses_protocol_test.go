package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const bareResponsesBody = `{"id":"resp_1","object":"response","model":"m","status":"completed",` +
	`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
	`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`

func newResponsesCompatExecutor(serverURL string) *OpenAICompatExecutor {
	return NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:     "zen",
			BaseURL:  serverURL + "/v1",
			Protocol: "responses",
		}},
	})
}

func responsesCompatAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     serverURL + "/v1",
			"api_key":      "test",
			"compat_name":  "zen",
			"provider_key": "zen",
		},
	}
}

func TestOpenAICompatExecutorResponsesProtocolNonStream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + bareResponsesBody + "}\n\n"))
	}))
	defer server.Close()

	executor := newResponsesCompatExecutor(server.URL)
	resp, err := executor.Execute(context.Background(), responsesCompatAuth(server.URL), cliproxyexecutor.Request{
		Model:   "muse-spark-1.3-contributor-free",
		Payload: []byte(`{"model":"muse-spark-1.3-contributor-free","messages":[{"role":"user","content":"say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/v1/responses")
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input in upstream body, got %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected messages in upstream body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("unexpected stream_options in upstream body: %s", string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("responses upstream must always be asked to stream (free-tier gate): %s", string(gotBody))
	}
	content := gjson.GetBytes(resp.Payload, "choices.0.message.content")
	if content.String() != "ok" {
		t.Fatalf("client content = %q, want %q; payload=%s", content.String(), "ok", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorResponsesProtocolStream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + bareResponsesBody + "}\n\n"))
	}))
	defer server.Close()

	executor := newResponsesCompatExecutor(server.URL)
	result, err := executor.ExecuteStream(context.Background(), responsesCompatAuth(server.URL), cliproxyexecutor.Request{
		Model:   "muse-spark-1.3-contributor-free",
		Payload: []byte(`{"model":"muse-spark-1.3-contributor-free","stream":true,"messages":[{"role":"user","content":"say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/v1/responses")
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("unexpected stream_options in upstream body: %s", string(gotBody))
	}

	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	if !strings.Contains(streamed.String(), "ok") {
		t.Fatalf("streamed payload missing content: %q", streamed.String())
	}
}

func TestOpenCodeZenHeaderAndAutoProtocolRouting(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyOpenCodeZenHeaders(req, "https://opencode.ai/zen/v1", "")

	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization = %q, want Bearer public", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "opencode/") {
		t.Fatalf("User-Agent = %q, want opencode/...", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, want cli", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q, want global", got)
	}
	if sess := req.Header.Get("x-opencode-session"); !strings.HasPrefix(sess, "ses_") || len(sess) != 30 {
		t.Fatalf("x-opencode-session = %q, want 30-char ses_...", sess)
	}
	if reqID := req.Header.Get("x-opencode-request"); !strings.HasPrefix(reqID, "msg_") || len(reqID) != 30 {
		t.Fatalf("x-opencode-request = %q, want 30-char msg_...", reqID)
	}

	// Verify model-based protocol selection under opencode.ai/zen
	if !providerUsesResponsesProtocol(nil, "muse-spark-1.3-contributor-free", "https://opencode.ai/zen/v1") {
		t.Fatal("muse-spark-1.3-contributor-free on opencode.ai/zen must select Responses protocol")
	}
	if providerUsesResponsesProtocol(nil, "big-pickle", "https://opencode.ai/zen/v1") {
		t.Fatal("big-pickle on opencode.ai/zen must remain on chat/completions protocol")
	}
	if !isOpenCodeZenSystemOneModel("jev-1.13-free") {
		t.Fatal("jev-1.13-free must be recognized as SystemOne model")
	}
}

func TestCompletedResponseEnvelope(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare object gets completed event", in: bareResponsesBody, want: "response.completed"},
		{name: "incomplete status keeps event type", in: `{"id":"r","status":"incomplete","output":[]}`, want: "response.incomplete"},
		{name: "typed payload passes through", in: `{"type":"response.completed","response":{"id":"r"}}`, want: "response.completed"},
		{name: "non json passes through", in: "not json", want: ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			out := completedResponseEnvelope([]byte(testCase.in))
			if got := gjson.GetBytes(out, "type").String(); got != testCase.want {
				t.Fatalf("type = %q, want %q; out=%s", got, testCase.want, string(out))
			}
		})
	}
}

func TestEnsureZenGateToolsAndChatGateTools(t *testing.T) {
	payload := ensureZenGateTools([]byte(`{"model":"muse-spark-1.3-contributor-free","stream":true,"input":[]}`))
	if !gjson.GetBytes(payload, "tools.#(name==bash)").Exists() || !gjson.GetBytes(payload, "tools.#(name==read)").Exists() {
		t.Fatalf("bash/read tool missing after Responses injection: %s", payload)
	}

	chatPayload := ensureZenChatGateTools([]byte(`{"model":"big-pickle","messages":[]}`))
	if !gjson.GetBytes(chatPayload, "tools.#(function.name==bash)").Exists() || !gjson.GetBytes(chatPayload, "tools.#(function.name==read)").Exists() {
		t.Fatalf("bash/read tool missing after Chat injection: %s", chatPayload)
	}
}

func TestReassembleChatCompletionsSSE(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":7,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"think\",\"content\":\"he\"}}]}\n\ndata: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":7,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	out, err := reassembleChatCompletionsSSE([]byte(sse))
	if err != nil {
		t.Fatalf("reassembleChatCompletionsSSE error: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("content not concatenated, got %q: %s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "think" {
		t.Fatalf("reasoning_content not preserved, got %q: %s", got, out)
	}
}
