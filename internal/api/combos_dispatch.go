// combos_dispatch.go wires the combos feature into the request path with the
// smallest possible surface: two route wrappers in server_routes.go plus a
// one-line snapshot sync inside pluginhost.Host.ApplyConfig. All combo logic
// lives in internal/combos.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)
// combosChatWrapper intercepts /v1/chat/completions when the requested model
// names a combo. Each member is attempted by rewriting ONLY the "model" field
// of the request body and invoking the normal handler; failures fall through
// per combos.ShouldFallbackStatus until a member answers successfully or the
// chain is exhausted (the last upstream error is passed through untouched).
//
// Streaming stays true-streaming: responses are held back only while the
// status is a fallback candidate; the moment a member returns 2xx its bytes
// pass straight through to the client.
func (s *Server) combosChatWrapper(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "could not read request body"})
			return
		}
		_ = c.Request.Body.Close()

		model := gjson.GetBytes(raw, "model").String()
		requiresVision := combos.RequestRequiresVision(raw)
		var chain []config.ComboModelRef

		combo, found := combos.Find(model)
		if found {
			chain = combos.Order(combo)
			// If request requires vision, prepend vision adapter models if members lack vision
			if requiresVision {
				hasVisionMember := false
				for _, m := range chain {
					if modelSupportsVision(m.Model) {
						hasVisionMember = true
						break
					}
				}
				if !hasVisionMember {
					visionPool := combos.GetVisionAdapterModels()
					if len(visionPool) > 0 {
						logrus.Infof("[router] 👁️ Vision adapter triggered for combo %q -> routing to %v first", combo.Name, visionPool)
						chain = append(visionPool, chain...)
					}
				}
			}
		} else if requiresVision && !modelSupportsVision(model) {
			// Single model request that cannot process vision -> auto-route to Vision Adapter Pool
			visionPool := combos.GetVisionAdapterModels()
			if len(visionPool) > 0 {
				logrus.Infof("[router] 👁️ Vision adapter triggered for single model %q -> auto-switching to %v", model, visionPool)
				chain = append(visionPool, config.ComboModelRef{Model: model})
			}
		}

		if len(chain) == 0 {
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			next(c)
			return
		}
		originalWriter := c.Writer
		var lastWriter *combosResponseWriter

		// Precompute maximum context length among candidate members
		maxContextInChain := 0
		for _, m := range chain {
			if info := registry.LookupStaticModelInfo(m.Model); info != nil && info.ContextLength > maxContextInChain {
				maxContextInChain = info.ContextLength
			}
		}

		for _, member := range chain {
			// OpenAI-compatible entries register bare model ids; routing picks
			// the serving provider itself. Members nothing can serve are
			// skipped so an unknown model cannot cut the chain short.
			providers := registry.GetGlobalRegistry().GetModelProviders(member.Model)
			if len(providers) == 0 {
				providers = registry.GetGlobalRegistry().GetModelProviders(combos.ModelID(member))
			}
			if len(providers) == 0 {
				logrus.WithField("combo", combo.Name).Warnf("[router] ⚠️ Combo %q skipping member %s: no provider serves model %q", combo.Name, combos.ModelID(member), member.Model)
				continue
			}

			// Context-Aware Guard: skip members whose context window cannot fit the payload,
			// provided there is a larger member in the chain that can fit it (or if all exceed,
			// preserve the largest tier models as best-effort so the request is not hard-dropped).
			if info := registry.LookupStaticModelInfo(member.Model); info != nil && info.ContextLength > 0 {
				estimatedTokens := len(raw) / 4
				if estimatedTokens > info.ContextLength {
					if maxContextInChain >= estimatedTokens || info.ContextLength < (maxContextInChain*9)/10 {
						logrus.WithField("combo", combo.Name).Warnf("[router] ⏩ Combo %q skipping member %s: estimated prompt tokens (%d) exceeds member context length (%d)", combo.Name, combos.ModelID(member), estimatedTokens, info.ContextLength)
						continue
					}
					logrus.WithField("combo", combo.Name).Infof("[router] ⚠️ Combo %q attempting member %s as best-effort: estimated prompt tokens (%d) exceeds member context length (%d)", combo.Name, combos.ModelID(member), estimatedTokens, info.ContextLength)
				}
			}
			reqEffort := strings.TrimSpace(gjson.GetBytes(raw, "reasoning_effort").String())
			if reqEffort == "" {
				reqEffort = strings.TrimSpace(gjson.GetBytes(raw, "reasoning.effort").String())
			}
			effortInfo := ""
			if reqEffort != "" {
				effortInfo = fmt.Sprintf(" [client_reasoning=%s]", reqEffort)
			}
			targetModel := member.Model
			logrus.Infof("[router] 🔀 Combo %q (%s)%s -> routing request to %s (candidates: %v)", combo.Name, combo.Strategy, effortInfo, combos.ModelID(member), providers)
			body, mErr := rewriteModelField(raw, targetModel)
			if mErr != nil {
				continue
			}
			req2 := c.Request.Clone(c.Request.Context())
			if member.Provider != "" {
				req2.Header.Set("X-Provider", member.Provider)
			}
			req2.Body = io.NopCloser(bytes.NewReader(body))
			req2.ContentLength = int64(len(body))
			c.Request = req2

			w := newCombosWriter(originalWriter)
			c.Writer = w
			next(c)
			c.Writer = originalWriter

			status := w.StatusCode()
			if status < http.StatusBadRequest {
				logrus.Infof("[router] ✅ Combo %q completed via %s (HTTP %d)", combo.Name, combos.ModelID(member), status)
				w.FlushHeld() // success: deliver whatever was streamed/buffered
				return
			}
			if !combos.ShouldFallback(status, w.held.Bytes()) {
				logrus.Warnf("[router] ❌ Combo %q client error on %s (HTTP %d): %s", combo.Name, combos.ModelID(member), status, string(w.held.Bytes()))
				w.FlushHeld() // definite client error: do not mask it
				return
			}
			logrus.Warnf("[router] ⚠️ Combo %q member %s returned HTTP %d -> falling back to next member", combo.Name, combos.ModelID(member), status)
			if w.Passthrough() {
				// Bytes already reached the client; cannot retry cleanly.
				return
			}
			lastWriter = w
		}

		if lastWriter != nil {
			lastWriter.FlushHeld() // expose the final upstream failure
		} else if len(chain) > 0 {
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": "combo \"" + combo.Name + "\" had no attemptable members", "type": "server_error"},
			})
		}
	}
}

// combosAugmentModels appends every configured combo to the /v1/models
// listing so IDEs can select them like any other model.
func (s *Server) combosAugmentModels(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Buffer the small JSON response so we can append entries and filter by API key policy.
		buf := &bytes.Buffer{}
		originalWriter := c.Writer
		grw := &passthroughRecorder{ResponseWriter: originalWriter, Body: buf}
		c.Writer = grw
		next(c)
		c.Writer = originalWriter

		var topMap map[string]any
		if err := json.Unmarshal(grw.Body.Bytes(), &topMap); err != nil {
			code := grw.heldCode
			if code <= 0 {
				code = http.StatusOK
			}
			c.Writer.WriteHeader(code)
			_, _ = c.Writer.Write(grw.Body.Bytes())
			return
		}
		obj, _ := topMap["object"].(string)
		rawList, hasData := topMap["data"].([]any)
		isAnthropic := isAnthropicModelsRequest(c) || (obj == "" && hasData)
		if !hasData || (obj != "list" && !isAnthropic) {
			code := grw.heldCode
			if code <= 0 {
				code = http.StatusOK
			}
			c.Writer.WriteHeader(code)
			_, _ = c.Writer.Write(grw.Body.Bytes())
			return
		}

		data := make([]map[string]any, 0, len(rawList))
		for _, item := range rawList {
			if m, ok := item.(map[string]any); ok {
				data = append(data, m)
			}
		}
		for _, cmb := range combos.Snapshot() {
			ctxLen, maxTok := ResolveComboDefaults(cmb)

			var entry map[string]any
			if isAnthropic {
				entry = map[string]any{
					"id":                    cmb.Name,
					"type":                  "model",
					"display_name":          cmb.Name,
					"created_at":            "2024-01-01T00:00:00Z",
					"context_length":        ctxLen,
					"max_context_length":    ctxLen,
					"max_input_tokens":      ctxLen,
					"max_tokens":            maxTok,
					"max_completion_tokens": maxTok,
				}
			} else {
				entry = map[string]any{
					"id":                    cmb.Name,
					"object":                "model",
					"owned_by":              "combos",
					"type":                  "combos",
					"context_length":        ctxLen,
					"max_context_length":    ctxLen,
					"inputTokenLimit":       ctxLen,
					"max_completion_tokens": maxTok,
					"max_tokens":            maxTok,
					"outputTokenLimit":      maxTok,
				}
			}
			data = append(data, entry)
		}

		// Also expose provider-namespaced IDs (e.g. agentrouter/gpt-6-astra, openagentic/gpt-6-astra, codex/gpt-6-astra, antigravity/gemini-3.8-flash-high)
		// so IDEs and tooling can explicitly choose a provider's model directly.
		existingIDs := make(map[string]struct{}, len(data))
		for _, d := range data {
			if id, ok := d["id"].(string); ok {
				existingIDs[id] = struct{}{}
			}
		}

		// 1. Expose provider-namespaced IDs for all serving providers from the registry
		for _, d := range data {
			id, okId := d["id"].(string)
			ownedBy, okOwn := d["owned_by"].(string)
			if !okId || id == "" || ownedBy == "combos" || strings.Contains(id, "/") {
				continue
			}
			for _, p := range registry.GetGlobalRegistry().GetModelProviders(id) {
				prov := strings.ToLower(strings.TrimPrefix(p, "openai-compatible-"))
				if prov == "" {
					continue
				}
				namespacedID := prov + "/" + id
				if _, exists := existingIDs[namespacedID]; !exists {
					entry := make(map[string]any, len(d))
					for k, v := range d {
						entry[k] = v
					}
					entry["id"] = namespacedID
					entry["owned_by"] = prov
					existingIDs[namespacedID] = struct{}{}
					data = append(data, entry)
				}
			}
			if okOwn && ownedBy != "" {
				prov := strings.ToLower(strings.TrimPrefix(ownedBy, "openai-compatible-"))
				namespacedID := prov + "/" + id
				if _, exists := existingIDs[namespacedID]; !exists {
					entry := make(map[string]any, len(d))
					for k, v := range d {
						entry[k] = v
					}
					entry["id"] = namespacedID
					existingIDs[namespacedID] = struct{}{}
					data = append(data, entry)
				}
			}
		}

		// 2. Explicitly expose all models from configured OpenAICompatibility providers (agentrouter, dahono, etc.)
		if s != nil && s.cfg != nil {
			for _, compat := range s.cfg.OpenAICompatibility {
				if compat.Disabled {
					continue
				}
				provName := strings.ToLower(strings.TrimSpace(compat.Name))
				if provName == "" {
					continue
				}
				for _, m := range compat.Models {
					mName := strings.TrimSpace(m.Name)
					if mName == "" {
						continue
					}
					bareName := strings.TrimPrefix(mName, provName+"/")
					namespacedID := provName + "/" + bareName

					ctxLen := 1000000
					maxTok := 128000
					if info := registry.LookupStaticModelInfo(bareName); info != nil {
						if info.ContextLength > 0 {
							ctxLen = info.ContextLength
						}
						if info.MaxCompletionTokens > 0 {
							maxTok = info.MaxCompletionTokens
						} else if info.OutputTokenLimit > 0 {
							maxTok = info.OutputTokenLimit
						}
					}

					if _, exists := existingIDs[namespacedID]; !exists {
						entry := map[string]any{
							"id":                    namespacedID,
							"object":                "model",
							"owned_by":              provName,
							"type":                  "openai-compatibility",
							"context_length":        ctxLen,
							"max_context_length":    ctxLen,
							"inputTokenLimit":       ctxLen,
							"max_completion_tokens": maxTok,
							"max_tokens":            maxTok,
							"outputTokenLimit":      maxTok,
						}
						if isAnthropic {
							entry["type"] = "model"
							entry["display_name"] = namespacedID
							entry["max_input_tokens"] = ctxLen
						}
						existingIDs[namespacedID] = struct{}{}
						data = append(data, entry)
					}

					if _, exists := existingIDs[bareName]; !exists {
						entry := map[string]any{
							"id":                    bareName,
							"object":                "model",
							"owned_by":              provName,
							"type":                  "openai-compatibility",
							"context_length":        ctxLen,
							"max_context_length":    ctxLen,
							"inputTokenLimit":       ctxLen,
							"max_completion_tokens": maxTok,
							"max_tokens":            maxTok,
							"outputTokenLimit":      maxTok,
						}
						if isAnthropic {
							entry["type"] = "model"
							entry["display_name"] = bareName
							entry["max_input_tokens"] = ctxLen
						}
						existingIDs[bareName] = struct{}{}
						data = append(data, entry)
					}
				}
			}
		}

		// Filter models based on the caller API key policy (if restricted)
		userKey := c.GetString("userApiKey")
		if userKey != "" {
			enforcer := apikeypolicy.Default()
			if policy, hasPolicy := enforcer.Policy(userKey); hasPolicy && (len(policy.Models) > 0 || len(policy.Providers) > 0) {
				filtered := make([]map[string]any, 0, len(data))
				for _, d := range data {
					id, _ := d["id"].(string)
					ownedBy, _ := d["owned_by"].(string)
					if id == "" {
						continue
					}
					if enforcer.IsModelAllowed(userKey, id, ownedBy) {
						filtered = append(filtered, d)
					}
				}
				data = filtered
			}
		}

		topMap["data"] = data
		out, _ := json.Marshal(topMap)
		if c.Writer.Header().Get("Content-Type") == "" {
			c.Writer.Header().Set("Content-Type", "application/json")
		}
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write(out)
	}
}

// passthroughRecorder HOLDS the response body (does not write through) so
// tiny JSON responses can be post-processed exactly once before delivery.
type passthroughRecorder struct {
	gin.ResponseWriter
	Body     *bytes.Buffer
	heldCode int
	wrote    bool
}

func (w *passthroughRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.heldCode = code
		w.wrote = true
	}
}

func (w *passthroughRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.Body.Write(b)
}

func (w *passthroughRecorder) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// rewriteModelField replaces only the top-level "model" field. Field order is
// not preserved — downstream parsing uses field lookups, not order.
func rewriteModelField(raw []byte, model string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	top["model"] = enc
	return json.Marshal(top)
}

// combosResponseWriter defers delivery while a fallback decision might still
// be made. Once a non-fallback status is observed it becomes passthrough so
// streaming members keep their live behaviour.
type combosResponseWriter struct {
	gin.ResponseWriter
	held        bytes.Buffer
	code        int
	passthrough bool
	wroteHeader bool
}

func newCombosWriter(w gin.ResponseWriter) *combosResponseWriter {
	return &combosResponseWriter{ResponseWriter: w}
}

func (w *combosResponseWriter) decide(code int) {
	w.code = code
	w.wroteHeader = true
	// Hold all error responses (HTTP >= 400) so fallback inspection can evaluate status and body.
	// Only successful responses (< 400) passthrough immediately for streaming.
	w.passthrough = code < http.StatusBadRequest
	if w.passthrough {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *combosResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.decide(code)
	if !w.passthrough {
		return // hold headers until flushed
	}
}

func (w *combosResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.decide(http.StatusOK)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(b)
	}
	return w.held.Write(b)
}

func (w *combosResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *combosResponseWriter) StatusCode() int {
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.code
}

func (w *combosResponseWriter) Passthrough() bool { return w.passthrough }

// FlushHeld releases held status+bytes to the real client (final accept).
func (w *combosResponseWriter) FlushHeld() {
	if w.passthrough {
		return
	}
	w.passthrough = true
	w.ResponseWriter.WriteHeader(w.code)
	if w.held.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.held.Bytes())
	}
}

var (
	_ gin.ResponseWriter = (*combosResponseWriter)(nil)
)

// ResolveComboDefaults resolves the context length and max output tokens for a combo.
// It inherits from the primary member (first model in the chain) when not explicitly set,
// adapting dynamically whether the primary model is Gemini (1M), Claude (1M/200k), or Codex (272k).
func ResolveComboDefaults(cmb config.ComboConfig) (int, int) {
	ctxLen := cmb.ContextLength
	maxTok := cmb.MaxTokens
	if maxTok <= 0 {
		maxTok = cmb.MaxCompletionTokens
	}

	// 1. Resolve Context Length
	if ctxLen <= 0 {
		if len(cmb.Models) > 0 {
			primary := cmb.Models[0]
			if info := registry.LookupStaticModelInfo(primary.Model); info != nil && info.ContextLength > 0 {
				ctxLen = info.ContextLength
			} else {
				m := strings.ToLower(primary.Model)
				if strings.Contains(m, "gemini") {
					ctxLen = 1048576
				} else if strings.Contains(m, "claude-opus") {
					ctxLen = 1000000
				} else if strings.Contains(m, "claude") {
					ctxLen = 200000
				} else if strings.Contains(m, "gpt-6") || strings.Contains(m, "gpt-5") || strings.Contains(m, "astra") || strings.Contains(m, "sol") || strings.Contains(m, "luna") {
					ctxLen = 272000
				}
			}
		}
		if ctxLen <= 0 {
			ctxLen = 1000000
		}
	}

	// 2. Resolve Max Completion Tokens
	if maxTok <= 0 {
		if len(cmb.Models) > 0 {
			primary := cmb.Models[0]
			if info := registry.LookupStaticModelInfo(primary.Model); info != nil {
				if info.MaxCompletionTokens > 0 {
					maxTok = info.MaxCompletionTokens
				} else if info.OutputTokenLimit > 0 {
					maxTok = info.OutputTokenLimit
				}
			}
		}
		if maxTok <= 0 {
			maxTok = 128000
		}
	}
	return ctxLen, maxTok
}

func modelSupportsVision(model string) bool {
	bare := model
	if _, after, ok := strings.Cut(model, "/"); ok {
		bare = after
	}

	// 1. Authoritative check: Model registry metadata (SupportedInputModalities)
	if info := registry.LookupStaticModelInfo(bare); info != nil && len(info.SupportedInputModalities) > 0 {
		for _, mod := range info.SupportedInputModalities {
			if strings.EqualFold(mod, "image") {
				return true
			}
		}
		return false
	}
	if info := registry.GetGlobalRegistry().GetModelInfo(bare, ""); info != nil && len(info.SupportedInputModalities) > 0 {
		for _, mod := range info.SupportedInputModalities {
			if strings.EqualFold(mod, "image") {
				return true
			}
		}
		return false
	}

	// 2. Model family heuristics for dynamic/unregistered/custom models
	return combos.HasVisionCapability(model) || combos.HasVisionCapability(bare)
}
