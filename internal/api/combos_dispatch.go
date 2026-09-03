// combos_dispatch.go wires the combos feature into the request path with the
// smallest possible surface: two route wrappers in server_routes.go plus a
// one-line snapshot sync inside pluginhost.Host.ApplyConfig. All combo logic
// lives in internal/combos.
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
					if combos.HasVisionCapability(m.Model) {
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
		} else if requiresVision && !combos.HasVisionCapability(model) {
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

		for _, member := range chain {
			// OpenAI-compatible entries register bare model ids; routing picks
			// the serving provider itself. Members nothing can serve are
			// skipped so an unknown model cannot cut the chain short.
			providers := registry.GetGlobalRegistry().GetModelProviders(member.Model)
			if len(providers) == 0 {
				logrus.WithField("combo", combo.Name).Warnf("[router] ⚠️ Combo %q skipping member %s: no provider serves model %q", combo.Name, combos.ModelID(member), member.Model)
				continue
			}
			logrus.Infof("[router] 🔀 Combo %q (%s) -> routing request to %s (candidates: %v)", combo.Name, combo.Strategy, combos.ModelID(member), providers)
			body, mErr := rewriteModelField(raw, member.Model)
			if mErr != nil {
				continue
			}
			req2 := c.Request.Clone(c.Request.Context())
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
			if !combos.ShouldFallbackStatus(status) {
				logrus.Warnf("[router] ❌ Combo %q client error on %s (HTTP %d)", combo.Name, combos.ModelID(member), status)
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
		if combos.SnapshotCount() == 0 {
			next(c)
			return
		}
		// Buffer the small JSON response so we can append entries.
		buf := &bytes.Buffer{}
		originalWriter := c.Writer
		grw := &passthroughRecorder{ResponseWriter: originalWriter, Body: buf}
		c.Writer = grw
		next(c)
		c.Writer = originalWriter

		var parsed struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(grw.Body.Bytes(), &parsed); err != nil || (parsed.Object != "list" && parsed.Data == nil) {
			code := grw.heldCode
			if code <= 0 {
				code = http.StatusOK
			}
			c.Writer.WriteHeader(code)
			_, _ = c.Writer.Write(grw.Body.Bytes())
			return
		}
		for _, cmb := range combos.Snapshot() {
			entry := map[string]any{
				"id":       cmb.Name,
				"object":   "model",
				"owned_by": "combos",
				"type":     "combos",
			}
			ctxLen := cmb.ContextLength
			maxTok := cmb.MaxTokens
			if maxTok <= 0 {
				maxTok = cmb.MaxCompletionTokens
			}

			// If not explicitly configured, inherit from primary/fallback model
			if len(cmb.Models) > 0 {
				primary := cmb.Models[0]
				info := registry.LookupStaticModelInfo(primary.Model)
				if info != nil {
					if ctxLen <= 0 && info.ContextLength > 0 {
						ctxLen = info.ContextLength
					}
					if maxTok <= 0 {
						if info.MaxCompletionTokens > 0 {
							maxTok = info.MaxCompletionTokens
						} else if info.OutputTokenLimit > 0 {
							maxTok = info.OutputTokenLimit
						}
					}
				} else {
					// Fallback default: 1,000,000 (1M) context length and 128,000 (128k) max completion tokens
					if ctxLen <= 0 {
						ctxLen = 1000000
					}
					if maxTok <= 0 {
						maxTok = 128000
					}
				}
			}
			if ctxLen > 0 {
				entry["context_length"] = ctxLen
				entry["max_context_length"] = ctxLen
				entry["inputTokenLimit"] = ctxLen
			}
			if maxTok > 0 {
				entry["max_completion_tokens"] = maxTok
				entry["max_tokens"] = maxTok
				entry["outputTokenLimit"] = maxTok
			}
			parsed.Data = append(parsed.Data, entry)
		}
		out, _ := json.Marshal(parsed)
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
	w.passthrough = !(code >= http.StatusBadRequest && combos.ShouldFallbackStatus(code))
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
