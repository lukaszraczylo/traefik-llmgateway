package traefikllmgateway

import "fmt"

// ThinkingConfig holds a provider's opt-in thinking adjustments; a nil one leaves bodies unchanged.
type ThinkingConfig struct {
	// OffBelowMaxTokens turns thinking off for a request that caps its output
	// under this many tokens and does not set thinking itself. 0 is off.
	OffBelowMaxTokens int64 `json:"offBelowMaxTokens,omitempty"`
}

const (
	chatTemplateKwargsField = "chat_template_kwargs"
	enableThinkingField     = "enable_thinking"
)

// thinkingFields are the request fields through which a client picks thinking itself.
var thinkingFields = []string{"reasoning_effort", "reasoning", "thinking", enableThinkingField}

func validateThinking(name, providerType string, tc *ThinkingConfig) error {
	if tc == nil {
		return nil
	}
	if providerType != providerTypeOpenAI {
		return fmt.Errorf("llmgateway: provider %q: thinking is supported for type %q only", name, providerTypeOpenAI)
	}
	if tc.OffBelowMaxTokens < 0 {
		return fmt.Errorf("llmgateway: provider %q: thinking.offBelowMaxTokens must not be negative, got %d", name, tc.OffBelowMaxTokens)
	}
	return nil
}

// providerThinkingFloor returns the provider's thinking.offBelowMaxTokens, 0 when unset.
func (g *Gateway) providerThinkingFloor(name string) int64 {
	if g == nil || g.cfg == nil {
		return 0
	}
	pc := g.cfg.Providers[name]
	if pc == nil || pc.Thinking == nil {
		return 0
	}
	return pc.Thinking.OffBelowMaxTokens
}

// clientSetsThinking reports whether req chooses thinking itself.
func clientSetsThinking(req map[string]any) bool {
	for i := 0; i < len(thinkingFields); i++ {
		if _, ok := req[thinkingFields[i]]; ok {
			return true
		}
	}
	kw, _ := req[chatTemplateKwargsField].(map[string]any)
	if kw == nil {
		return false
	}
	_, enable := kw[enableThinkingField]
	_, think := kw["thinking"]
	return enable || think
}

// applyThinkingFloor turns thinking off in the OpenAI-shaped body when the
// client in req caps its output under floor and leaves thinking unset: a
// thinking model spends a small cap on reasoning and returns empty content.
func applyThinkingFloor(body, req map[string]any, floor int64) {
	if floor <= 0 || clientSetsThinking(req) {
		return
	}
	n := requestedOutputTokens(body, cacheEndpointChat)
	if n <= 0 || n >= floor {
		return
	}
	kw := map[string]any{enableThinkingField: false}
	if prev, ok := body[chatTemplateKwargsField].(map[string]any); ok {
		for k, v := range prev {
			kw[k] = v
		}
		kw[enableThinkingField] = false
	}
	body[chatTemplateKwargsField] = kw
}
