package contextcompression

import "encoding/json"

const maxTARESlotBytes = 1024 * 1024

type slot struct {
	text, class string
	write       func(string)
}

func collectSlots(body any, capBytes int) []slot {
	root, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	var slots []slot
	add := func(owner map[string]any, key, class string) {
		text, ok := owner[key].(string)
		if ok && text != "" && capBytes > 0 && len([]byte(text)) <= capBytes {
			slots = append(slots, slot{text, class, func(v string) { owner[key] = v }})
		}
	}
	var items []any
	if v, ok := root["messages"].([]any); ok {
		items = v
	} else if v, ok := root["input"].([]any); ok {
		items = v
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "function_call_output" {
			if _, ok := m["output"].(string); ok {
				add(m, "output", "responses-string")
			} else {
				addTextParts(m["output"], "input_text", "responses-input-text", add)
			}
			continue
		}
		if m["role"] == "tool" {
			if _, ok := m["content"].(string); ok {
				add(m, "content", "openai-tool-string")
			} else {
				addTextParts(m["content"], "text", "openai-tool-text", add)
			}
			continue
		}
		blocks, _ := m["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			if block["type"] != "tool_result" || block["is_error"] == true {
				continue
			}
			if _, ok := block["content"].(string); ok {
				add(block, "content", "claude-tool-result-string")
			} else {
				addTextParts(block["content"], "text", "claude-tool-result-text", add)
			}
		}
	}
	if state, ok := root["conversationState"].(map[string]any); ok {
		collectKiro(state, add)
	}
	collectGemini(root["contents"], add)
	if req, ok := root["request"].(map[string]any); ok {
		collectGemini(req["contents"], add)
	}
	return slots
}

func addTextParts(raw any, typ, class string, add func(map[string]any, string, string)) {
	parts, _ := raw.([]any)
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["type"] == typ {
			add(m, "text", class)
		}
	}
}
func collectKiro(state map[string]any, add func(map[string]any, string, string)) {
	messages, _ := state["history"].([]any)
	if cur := state["currentMessage"]; cur != nil {
		messages = append(messages, cur)
	}
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		u, _ := m["userInputMessage"].(map[string]any)
		c, _ := u["userInputMessageContext"].(map[string]any)
		results, _ := c["toolResults"].([]any)
		for _, rr := range results {
			r, _ := rr.(map[string]any)
			if r["status"] == "error" {
				continue
			}
			parts, _ := r["content"].([]any)
			for _, pp := range parts {
				p, _ := pp.(map[string]any)
				add(p, "text", "kiro-tool-result")
			}
		}
	}
}
func collectGemini(raw any, add func(map[string]any, string, string)) {
	contents, _ := raw.([]any)
	for _, cc := range contents {
		c, _ := cc.(map[string]any)
		parts, _ := c["parts"].([]any)
		for _, pp := range parts {
			p, _ := pp.(map[string]any)
			f, _ := p["functionResponse"].(map[string]any)
			if f == nil {
				continue
			}
			if _, ok := f["response"].(string); ok {
				add(f, "response", "gemini-function-response")
				continue
			}
			r, _ := f["response"].(map[string]any)
			if _, ok := r["result"].(string); ok {
				add(r, "result", "gemini-result")
				continue
			}
			w, _ := r["result"].(map[string]any)
			add(w, "result", "gemini-result-wrapper")
		}
	}
}

func marshalBlock(s slot) []byte {
	b, _ := json.Marshal([]map[string]string{{"role": "tool", "kind": "tool_output", "class": "ainyrouter-" + s.class, "text": s.text}})
	return b
}
