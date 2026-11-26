package traefikllmgateway

import "fmt"

// PromptCacheConfig holds a provider's opt-in prompt-caching body changes.
type PromptCacheConfig struct {
	// Inject adds Anthropic cache_control breakpoints. anthropic type only.
	Inject bool `json:"inject,omitempty"`
	// Key adds a hashed prompt_cache_key. openai type only.
	Key bool `json:"key,omitempty"`
}

// maxCacheBreakpoints is Anthropic's limit on cache_control markers per request.
const maxCacheBreakpoints = 4

// providerPromptCache returns the provider's promptCache config, or the
// zero value when the provider or the block is absent.
func (g *Gateway) providerPromptCache(name string) PromptCacheConfig {
	if g == nil || g.cfg == nil {
		return PromptCacheConfig{}
	}
	pc := g.cfg.Providers[name]
	if pc == nil || pc.PromptCache == nil {
		return PromptCacheConfig{}
	}
	return *pc.PromptCache
}

func validatePromptCache(name, providerType string, pc *PromptCacheConfig) error {
	if pc == nil {
		return nil
	}
	if pc.Inject && providerType != providerTypeAnthropic {
		return fmt.Errorf("llmgateway: provider %q: promptCache.inject is supported for type %q only", name, providerTypeAnthropic)
	}
	if pc.Key && providerType != providerTypeOpenAI {
		return fmt.Errorf("llmgateway: provider %q: promptCache.key is supported for type %q only", name, providerTypeOpenAI)
	}
	return nil
}

func newCacheMarker() map[string]any {
	return map[string]any{"type": "ephemeral"}
}

// hasCacheControl reports whether any map under v carries a cache_control key.
func hasCacheControl(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["cache_control"]; ok {
			return true
		}
		for _, c := range t {
			if hasCacheControl(c) {
				return true
			}
		}
	case []any:
		for i := 0; i < len(t); i++ {
			if hasCacheControl(t[i]) {
				return true
			}
		}
	}
	return false
}

// markBlock returns a copy of block carrying a cache_control marker.
func markBlock(block any) (any, bool) {
	m, ok := block.(map[string]any)
	if !ok {
		return block, false
	}
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	cp["cache_control"] = newCacheMarker()
	return cp, true
}

// markLastBlock returns a copy of blocks with the last entry marked.
func markLastBlock(blocks []any) ([]any, bool) {
	if len(blocks) == 0 {
		return blocks, false
	}
	marked, ok := markBlock(blocks[len(blocks)-1])
	if !ok {
		return blocks, false
	}
	cp := make([]any, len(blocks))
	copy(cp, blocks)
	cp[len(cp)-1] = marked
	return cp, true
}

// markContent returns content (string or block list) with its last block marked.
func markContent(content any) (any, bool) {
	switch c := content.(type) {
	case string:
		if c == "" {
			return content, false
		}
		return []any{map[string]any{"type": "text", "text": c, "cache_control": newCacheMarker()}}, true
	case []any:
		out, ok := markLastBlock(c)
		return out, ok
	}
	return content, false
}

// markMessage returns a copy of messages with entry idx's last content block marked.
func markMessage(messages []any, idx int) ([]any, bool) {
	msg, ok := messages[idx].(map[string]any)
	if !ok {
		return messages, false
	}
	content, ok := markContent(msg["content"])
	if !ok {
		return messages, false
	}
	msgCopy := make(map[string]any, len(msg))
	for k, v := range msg {
		msgCopy[k] = v
	}
	msgCopy["content"] = content
	cp := make([]any, len(messages))
	copy(cp, messages)
	cp[idx] = msgCopy
	return cp, true
}

// injectCacheControl adds up to four cache breakpoints to an Anthropic-format
// body: last tool, last system block, and the last two user messages. It
// never mutates req; touched nodes are copied. A body that already carries
// any cache_control is returned unchanged.
func injectCacheControl(req map[string]any) (map[string]any, int) {
	if hasCacheControl(req["tools"]) || hasCacheControl(req["system"]) || hasCacheControl(req["messages"]) {
		return req, 0
	}
	out := make(map[string]any, len(req))
	for k, v := range req {
		out[k] = v
	}
	added := 0

	if tools, ok := req["tools"].([]any); ok {
		if marked, done := markLastBlock(tools); done {
			out["tools"] = marked
			added++
		}
	}
	if system, done := markContent(req["system"]); done {
		out["system"] = system
		added++
	}
	if messages, ok := req["messages"].([]any); ok {
		users := make([]int, 0, 2)
		for i := len(messages) - 1; i >= 0 && len(users) < 2; i-- {
			if m, isMap := messages[i].(map[string]any); isMap && m["role"] == "user" {
				users = append(users, i)
			}
		}
		cur := messages
		for _, idx := range users {
			if marked, done := markMessage(cur, idx); done {
				cur = marked
				added++
			}
		}
		out["messages"] = cur
	}
	if added > maxCacheBreakpoints {
		return req, 0
	}
	return out, added
}
