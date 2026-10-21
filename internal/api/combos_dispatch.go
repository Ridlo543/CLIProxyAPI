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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
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
			// Preserve the status the body limit implies: an oversized chunked
			// request must surface as 413, not as a generic 400.
			c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), gin.H{"error": "could not read request body"})
			return
		}
		_ = c.Request.Body.Close()

		model := gjson.GetBytes(raw, "model").String()
		requiresVision := combos.RequestRequiresVision(raw)
		var chain []config.ComboModelRef

		combo, found := combos.Find(model)
		if found {
			chain = combos.Order(combo)
			// Vision requests get capability-ordered members: everything that can
			// read images first (stable, no member is dropped), so the chain does
			// not waste an attempt on a text-only member. Only when no member can
			// see images is the adapter pool prepended.
			if requiresVision {
				ordered, hasVisionMember := reorderVisionFirst(chain)
				chain = ordered
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

		// Precompute the largest effective context window in the chain. The
		// registry carries the config-resolved value for openai-compatibility
		// members, which the static catalog knows nothing about.
		maxContextInChain := 0
		for _, m := range chain {
			if ctx := effectiveContextLength(m); ctx > maxContextInChain {
				maxContextInChain = ctx
			}
		}
		estimatedTokens := estimateComboPromptTokens(raw)
		policy := combos.HealthPolicy()
		startedAt := time.Now()

		type attemptFailure struct {
			writer     *combosResponseWriter
			member     config.ComboModelRef
			status     int
			retryAfter time.Duration
		}
		var failures []attemptFailure
		attempts := 0
		skippedCooling := 0

		// attemptMember performs one member attempt and returns the held writer.
		// A nil writer means the member was not attemptable (no serving provider
		// or an unrewritable body).
		attemptMember := func(member config.ComboModelRef) (*combosResponseWriter, int, []string) {
			var providers []string
			if member.Provider != "" {
				providers = registry.GetGlobalRegistry().GetModelProviders(combos.ModelID(member))
			}
			if len(providers) == 0 {
				providers = registry.GetGlobalRegistry().GetModelProviders(member.Model)
			}
			if len(providers) == 0 {
				logrus.WithField("combo", combo.Name).Warnf("[router] ⚠️ Combo %q skipping member %s: no provider serves model %q", combo.Name, combos.ModelID(member), member.Model)
				return nil, 0, nil
			}

			reqEffort := strings.TrimSpace(gjson.GetBytes(raw, "reasoning_effort").String())
			if reqEffort == "" {
				reqEffort = strings.TrimSpace(gjson.GetBytes(raw, "reasoning.effort").String())
			}
			effortInfo := ""
			if reqEffort != "" {
				effortInfo = fmt.Sprintf(" [client_reasoning=%s]", reqEffort)
			}
			logrus.Infof("[router] 🔀 Combo %q (%s)%s -> routing request to %s (candidates: %v)", combo.Name, combo.Strategy, effortInfo, combos.ModelID(member), providers)
			body, mErr := rewriteModelField(raw, member.Model)
			if mErr != nil {
				logrus.WithField("combo", combo.Name).Warnf("[router] ⚠️ Combo %q could not rewrite body for %s: %v", combo.Name, combos.ModelID(member), mErr)
				return nil, 0, providers
			}
			req2 := c.Request.Clone(c.Request.Context())
			if member.Provider != "" {
				req2.Header.Set("X-Provider", member.Provider)
			}
			req2.Body = io.NopCloser(bytes.NewReader(body))
			req2.ContentLength = int64(len(body))
			c.Request = req2

			// Headers live on the shared underlying writer; a stale Retry-After
			// from the previous member must not be attributed to this one.
			originalWriter.Header().Del("Retry-After")

			w := newCombosWriter(originalWriter)
			c.Writer = w
			next(c)
			c.Writer = originalWriter
			return w, w.StatusCode(), providers
		}

		for _, member := range chain {
			// Context-Aware Guard: skip members whose context window cannot fit
			// the payload, provided a larger member in the chain can fit it (or
			// if all exceed, keep the largest tier as best-effort so the request
			// is not hard-dropped).
			if memberCtx := effectiveContextLength(member); memberCtx > 0 && estimatedTokens > memberCtx {
				if maxContextInChain >= estimatedTokens || memberCtx < (maxContextInChain*9)/10 {
					logrus.WithField("combo", combo.Name).Warnf("[router] ⏩ Combo %q skipping member %s: estimated prompt tokens (%d) exceeds member context length (%d)", combo.Name, combos.ModelID(member), estimatedTokens, memberCtx)
					continue
				}
				logrus.WithField("combo", combo.Name).Infof("[router] ⚠️ Combo %q attempting member %s as best-effort: estimated prompt tokens (%d) exceeds member context length (%d)", combo.Name, combos.ModelID(member), estimatedTokens, memberCtx)
			}

			// Budget guard: never abort an attempt already in flight, but stop
			// stacking new attempts once the request budget is spent.
			if attempts > 0 && policy.RequestBudget > 0 && time.Since(startedAt) >= policy.RequestBudget {
				logrus.WithField("combo", combo.Name).Warnf("[router] ⏱️ Combo %q request budget %s exhausted after %d attempt(s); not starting more members", combo.Name, policy.RequestBudget, attempts)
				break
			}

			// Circuit breaker: an open member is skipped in microseconds instead
			// of burning another first-token timeout.
			allowed, remaining := combos.AllowAttempt(member.Provider, member.Model, time.Now())
			if !allowed {
				skippedCooling++
				logrus.WithField("combo", combo.Name).Warnf("[router] 🧊 Combo %q skipping member %s: cooling down (%s left)", combo.Name, combos.ModelID(member), remaining.Round(time.Second))
				continue
			}

			w, status, _ := attemptMember(member)
			if w == nil {
				// The member could not be attempted (no serving provider or an
				// unrewritable body). Release the half-open reservation so it is
				// not stuck skipping that member forever.
				combos.RecordSuccess(member.Provider, member.Model)
				continue
			}
			attempts++

			if status < http.StatusBadRequest {
				logrus.Infof("[router] ✅ Combo %q completed via %s (HTTP %d)", combo.Name, combos.ModelID(member), status)
				combos.RecordSuccess(member.Provider, member.Model)
				w.FlushHeld() // success: deliver whatever was streamed/buffered
				return
			}
			bodyBytes := w.held.Bytes()

			// A client-side fault no other member can fix: return it as-is and
			// never open the member (another request may still succeed).
			if combos.IsPoisonRequest(status, bodyBytes) {
				logrus.Warnf("[router] ❌ Combo %q request rejected by %s (HTTP %d, request-scoped): %s", combo.Name, combos.ModelID(member), status, string(bodyBytes))
				combos.RecordSuccess(member.Provider, member.Model)
				w.FlushHeld()
				return
			}
			if !combos.ShouldFallback(status, bodyBytes) {
				logrus.Warnf("[router] ❌ Combo %q client error on %s (HTTP %d): %s", combo.Name, combos.ModelID(member), status, string(bodyBytes))
				w.FlushHeld() // definite client error: do not mask it
				return
			}

			retryAfter := combos.ExtractRetryAfter(status, w.ResponseWriter.Header().Get("Retry-After"), bodyBytes)
			cooldown := combos.RecordFailure(member.Provider, member.Model, status, bodyBytes, time.Now(), retryAfter)
			if cooldown > 0 {
				logrus.Warnf("[router] ⚠️ Combo %q member %s returned HTTP %d -> cooling %s, falling back to next member", combo.Name, combos.ModelID(member), status, cooldown)
			} else {
				logrus.Warnf("[router] ⚠️ Combo %q member %s returned HTTP %d -> falling back to next member (no cooldown: provider opts out)", combo.Name, combos.ModelID(member), status)
			}
			failures = append(failures, attemptFailure{writer: w, member: member, status: status, retryAfter: retryAfter})
			if w.Passthrough() {
				// Bytes already reached the client; cannot retry cleanly.
				return
			}
		}

		// Every member was cooling down. Instead of failing outright, probe the
		// member that recovers first so a chain never gets stuck behind a single
		// cooled member (the probe result decides whether it reopens).
		if attempts == 0 && skippedCooling > 0 && policy.BestEffortWhenAllOpen {
			if rescue, until, ok := combos.SoonestCooling(chain, time.Now()); ok {
				logrus.WithField("combo", combo.Name).Warnf("[router] 🩺 Combo %q all members cooling; best-effort probe of %s (recovers in %s)", combo.Name, combos.ModelID(rescue), time.Until(until).Round(time.Second))
				combos.ForceAttempt(rescue.Provider, rescue.Model)
				w, status, _ := attemptMember(rescue)
				if w == nil {
					combos.RecordSuccess(rescue.Provider, rescue.Model)
				}
				if w != nil {
					attempts++
					if status < http.StatusBadRequest {
						logrus.Infof("[router] ✅ Combo %q recovered via %s (HTTP %d)", combo.Name, combos.ModelID(rescue), status)
						combos.RecordSuccess(rescue.Provider, rescue.Model)
						w.FlushHeld()
						return
					}
					bodyBytes := w.held.Bytes()
					if !combos.ShouldFallback(status, bodyBytes) {
						// Request-scoped fault: the member itself is fine.
						combos.RecordSuccess(rescue.Provider, rescue.Model)
						w.FlushHeld()
						return
					}
					retryAfter := combos.ExtractRetryAfter(status, w.ResponseWriter.Header().Get("Retry-After"), bodyBytes)
					cooldown := combos.RecordFailure(rescue.Provider, rescue.Model, status, bodyBytes, time.Now(), retryAfter)
					logrus.Warnf("[router] ⚠️ Combo %q probe of %s failed (HTTP %d), reopened for %s", combo.Name, combos.ModelID(rescue), status, cooldown)
					failures = append(failures, attemptFailure{writer: w, member: rescue, status: status, retryAfter: retryAfter})
				}
			}
		}

		if len(failures) > 0 {
			// Surface the failure that asks for the shortest wait: returning the
			// last member's multi-hour retry hint would pin the client for hours
			// even though another member could serve it minutes from now.
			// A member that sent no hint is retryable immediately, so it is the
			// shortest wait of all; otherwise pick the smallest hint. Never surface
			// the longest one just because it was last.
			chosen := failures[len(failures)-1]
			for _, f := range failures {
				if f.retryAfter == 0 {
					chosen = f
					continue
				}
				if chosen.retryAfter > 0 && f.retryAfter < chosen.retryAfter {
					chosen = f
				}
			}
			writeHeldFailure(c, chosen.writer, chosen.retryAfter, policy.RetryAfterCap)
			return
		}

		if attempts == 0 {
			// Nothing was even attemptable (cooldowns disabled the chain and the
			// rescue probe was skipped). Tell the client when to come back.
			if _, until, ok := combos.SoonestCooling(chain, time.Now()); ok {
				retry := time.Until(until)
				if retry < 0 {
					retry = 0
				}
				if policy.RetryAfterCap > 0 && retry > policy.RetryAfterCap {
					retry = policy.RetryAfterCap
				}
				c.Header("Retry-After", strconv.FormatInt(int64(retry.Seconds()), 10))
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{
						"message":        "combo \"" + combo.Name + "\": every member is cooling down",
						"type":           "server_error",
						"retry_after_ms": retry.Milliseconds(),
					},
				})
				return
			}
		}

		if len(chain) > 0 {
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

// maxHeldErrorBytes bounds the buffered error body. Fallback inspection only
// needs the status and a short prefix; buffering an unbounded upstream error (or
// a proxy HTML page) would let one bad member grow memory per request.
const maxHeldErrorBytes = 1 << 20

func (w *combosResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.decide(http.StatusOK)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(b)
	}
	written := len(b)
	if remaining := maxHeldErrorBytes - w.held.Len(); remaining <= 0 {
		return written, nil // discard the tail but report success to the caller
	} else if len(b) > remaining {
		_, _ = w.held.Write(b[:remaining])
		return written, nil
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

// effectiveContextLength resolves the context window a member can actually
// accept. The static catalog covers OAuth channels only, so the registry (which
// is built from config, including openai-compatibility max-context-length) is
// consulted first and the static catalog is the fallback.
func effectiveContextLength(member config.ComboModelRef) int {
	bare := member.Model
	if _, after, ok := strings.Cut(bare, "/"); ok {
		bare = after
	}
	best := 0
	consider := func(info *registry.ModelInfo) {
		if info == nil {
			return
		}
		if info.ContextLength > best {
			best = info.ContextLength
		}
		if info.MaxContextLength > best {
			best = info.MaxContextLength
		}
	}
	reg := registry.GetGlobalRegistry()
	if reg != nil {
		consider(reg.GetModelInfo(member.Model, member.Provider))
		if bare != member.Model {
			consider(reg.GetModelInfo(bare, member.Provider))
		}
	}
	consider(registry.LookupStaticModelInfo(member.Model))
	if bare != member.Model {
		consider(registry.LookupStaticModelInfo(bare))
	}
	return best
}

// estimateComboPromptTokens estimates the prompt size for the context guard.
// It counts extractable text conservatively (low bytes-per-token) so a member is
// only skipped when the payload clearly exceeds its window.
func estimateComboPromptTokens(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var chars int64
	var addText func(v gjson.Result, key string)
	addText = func(v gjson.Result, key string) {
		if mediaPayloadKeys[strings.ToLower(key)] {
			return // inline image/audio bytes are not prompt text
		}
		if v.Type == gjson.String {
			text := v.String()
			if strings.HasPrefix(text, "data:") {
				return // data URI carrying inline media
			}
			chars += int64(len(text))
		}
		if v.IsArray() {
			for _, item := range v.Array() {
				addText(item, key)
			}
		}
		if v.IsObject() {
			for k, item := range v.Map() {
				addText(item, k)
			}
		}
	}
	addText(gjson.GetBytes(raw, "messages"), "messages")
	addText(gjson.GetBytes(raw, "input"), "input")
	addText(gjson.GetBytes(raw, "system"), "system")
	addText(gjson.GetBytes(raw, "contents"), "contents")
	if chars == 0 {
		// Fall back to the raw body size when the shape is unknown.
		return len(raw) / 4
	}
	// ~3.5 chars per token for mixed ASCII/JSON, rounded up.
	return int(chars/3) + 1
}

// mediaPayloadKeys are field names whose string values carry inline binary media
// instead of prompt text. Counting them as text made every image request look
// like it exceeded every member's context window.
var mediaPayloadKeys = map[string]bool{
	"data":        true,
	"b64":         true,
	"b64_json":    true,
	"url":         true,
	"image_url":   true,
	"input_image": true,
	"inline_data": true,
	"inlinedata":  true,
	"file_data":   true,
	"filedata":    true,
	"audio":       true,
	"video":       true,
}

// writeHeldFailure releases a buffered error response to the client, clamping an
// oversized retry hint so one member's multi-hour Retry-After cannot pin the
// client when another member only needs a few minutes.
func writeHeldFailure(c *gin.Context, w *combosResponseWriter, retryAfter, cap time.Duration) {
	body := append([]byte(nil), w.held.Bytes()...)
	effective := retryAfter
	if cap > 0 && effective > cap {
		effective = cap
		body = clampRetryAfterJSON(body, cap)
	}
	if effective > 0 {
		// Make the wait deterministic: the header always matches the member whose
		// failure was surfaced, even if another member set it earlier.
		c.Writer.Header().Set("Retry-After", strconv.FormatInt(int64((effective+time.Second-1)/time.Second), 10))
	}
	status := w.code
	if status <= 0 {
		status = http.StatusBadGateway
	}
	c.Writer.WriteHeader(status)
	if len(body) > 0 {
		_, _ = c.Writer.Write(body)
	}
}

// clampRetryAfterJSON rewrites oversized retry hints inside an error body.
func clampRetryAfterJSON(body []byte, cap time.Duration) []byte {
	out := body
	clampMS := cap.Milliseconds()
	clampSec := int64(cap.Seconds())
	for _, field := range []string{"retry-after-ms", "retry_after_ms", "error.retry-after-ms", "error.retry_after_ms"} {
		if v := gjson.GetBytes(out, field); v.Exists() && v.Type == gjson.Number && v.Int() > clampMS {
			if next, err := sjson.SetBytes(out, field, clampMS); err == nil {
				out = next
			}
		}
	}
	for _, field := range []string{"retry_after", "error.retry_after"} {
		if v := gjson.GetBytes(out, field); v.Exists() && v.Type == gjson.Number && v.Int() > clampSec {
			if next, err := sjson.SetBytes(out, field, clampSec); err == nil {
				out = next
			}
		}
	}
	return out
}

// reorderVisionFirst stably partitions a chain into image-capable members
// followed by the rest. No member is dropped, so fallback coverage is unchanged,
// and the relative order inside each partition is preserved.
func reorderVisionFirst(chain []config.ComboModelRef) ([]config.ComboModelRef, bool) {
	capable := make([]config.ComboModelRef, 0, len(chain))
	rest := make([]config.ComboModelRef, 0, len(chain))
	for _, m := range chain {
		if modelSupportsVision(m.Model) {
			capable = append(capable, m)
			continue
		}
		rest = append(rest, m)
	}
	if len(capable) == 0 {
		return chain, false
	}
	return append(capable, rest...), true
}
