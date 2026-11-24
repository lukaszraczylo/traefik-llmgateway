package traefikllmgateway

import (
	"strings"
	"testing"
)

func newClassifyTestRouter(t testing.TB, mut func(*SmartRouterConfig)) *smartRouter {
	t.Helper()
	cfg := &SmartRouterConfig{Tiers: smartTestTiers()}
	if mut != nil {
		mut(cfg)
	}
	rt, err := compileSmartRouter("smart/auto", cfg, nil)
	if err != nil {
		t.Fatalf("compileSmartRouter: %v", err)
	}
	return rt
}

func chatMsgs(msgs ...any) map[string]any {
	return map[string]any{"messages": msgs}
}

func msg(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func part(typ, key, val string) map[string]any {
	return map[string]any{"type": typ, key: val}
}

const strongAsk = "Analyze the trade-off between kafka and postgres for a distributed architecture"

func TestClassifySmartRequest(t *testing.T) {
	t.Parallel()
	toolCall := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}}
	toolResult := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"}
	anthropicToolUse := msg("assistant", []any{map[string]any{"type": "tool_use", "id": "c1", "name": "f"}})
	anthropicToolResult := msg("user", []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "ok"}})

	tests := []struct {
		mut       func(*SmartRouterConfig)
		req       map[string]any
		name      string
		endpoint  string
		wantWhy   string
		wantText  string
		wantRank  int
		wantImage bool
	}{
		{
			name:     "trivial greeting stays cheapest",
			req:      chatMsgs(msg("user", "hello")),
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore, wantText: "hello",
		},
		{
			name:     "code request scores mid tier",
			req:      chatMsgs(msg("user", "```python\ndef f(): pass\n```\nplease debug this exception and refactor it")),
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonScore,
		},
		{
			name:     "two reasoning markers floor to top tier",
			req:      chatMsgs(msg("user", strongAsk)),
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name:     "long plain text only earns the length weight",
			req:      chatMsgs(msg("user", strings.Repeat("lorem ipsum ", 400))),
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore,
		},
		{
			name:     "string content on anthropic messages",
			req:      map[string]any{"system": "be brief", "messages": []any{msg("user", "hello")}},
			endpoint: cacheEndpointMessages,
			wantRank: 0, wantWhy: smartReasonScore, wantText: "hello\nbe brief",
		},
		{
			name: "content parts on anthropic messages",
			req: map[string]any{"messages": []any{
				msg("user", []any{part("text", "text", strongAsk)}),
			}},
			endpoint: cacheEndpointMessages,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name: "anthropic top-level system is read only on the messages endpoint",
			req:  map[string]any{"system": "secret prompt", "messages": []any{msg("user", "hello")}},

			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore, wantText: "hello",
		},
		{
			name: "openai tool loop scores the earlier real ask",
			req: chatMsgs(
				msg("user", strongAsk), toolCall, toolResult,
			),
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name: "openai tool loop on a trivial ask is floored to rank 1 and scores that ask",
			req: chatMsgs(
				msg("user", "hello"), toolCall, toolResult,
			),
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonFloor, wantText: "hello",
		},
		{
			name: "anthropic tool_result-only trailing turn scores the earlier ask",
			req: map[string]any{"messages": []any{
				msg("user", strongAsk), anthropicToolUse, anthropicToolResult,
			}},
			endpoint: cacheEndpointMessages,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name:     "no user text uses the default tier",
			mut:      func(c *SmartRouterConfig) { c.DefaultTier = "mid" },
			req:      chatMsgs(msg("assistant", "hi there")),
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonDefault,
		},
		{
			name:     "missing messages uses the first tier by default",
			req:      map[string]any{},
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonDefault,
		},
		{
			name: "tools floor raises a no-text request above the default",
			req: map[string]any{
				"messages": []any{msg("assistant", "x")},
				"tools":    []any{map[string]any{"type": "function"}},
			},
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonFloor,
		},
		{
			name: "tools present floors a trivial ask to rank 1",
			req: map[string]any{
				"messages": []any{msg("user", "hello")},
				"tools":    []any{map[string]any{"type": "function"}},
			},
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonFloor,
		},
		{
			name:     "reasoning_effort high goes to top",
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "reasoning_effort": "high"},
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name:     "reasoning_effort medium goes to top",
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "reasoning_effort": "Medium"},
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name:     "reasoning_effort low does not floor",
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "reasoning_effort": "low"},
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore,
		},
		{
			name:     "thinking enabled goes to top",
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}},
			endpoint: cacheEndpointMessages,
			wantRank: 2, wantWhy: smartReasonFloor,
		},
		{
			name:     "thinking disabled does not floor",
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "thinking": map[string]any{"type": "disabled"}},
			endpoint: cacheEndpointMessages,
			wantRank: 0, wantWhy: smartReasonScore,
		},
		{
			name: "keyword rule raises to its tier, case-insensitively",
			mut: func(c *SmartRouterConfig) {
				c.KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: []string{"GDPR"}}}
			},
			req:      chatMsgs(msg("user", "gdpr?")),
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonKeyword,
		},
		{
			name: "highest matching keyword rule wins",
			mut: func(c *SmartRouterConfig) {
				c.KeywordRules = []SmartRouterKeywordRule{
					{Tier: "top", Keywords: []string{"audit"}},
					{Tier: "mid", Keywords: []string{"invoice"}},
				}
			},
			req:      chatMsgs(msg("user", "invoice audit")),
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonKeyword,
		},
		{
			name: "keyword rule never lowers a higher score rank",
			mut: func(c *SmartRouterConfig) {
				c.KeywordRules = []SmartRouterKeywordRule{{Tier: "cheap", Keywords: []string{"debug"}}}
			},
			req:      chatMsgs(msg("user", "```go\nfunc f() {}\n```\ndebug this exception and refactor")),
			endpoint: cacheEndpointChat,
			wantRank: 1, wantWhy: smartReasonScore,
		},
		{
			name: "floor never lowers a top score",
			req: map[string]any{
				"messages": []any{msg("user", strongAsk+" step by step with a ```code``` sample to debug and implement")},
				"tools":    []any{map[string]any{"type": "function"}},
			},
			endpoint: cacheEndpointChat,
			wantRank: 2, wantWhy: smartReasonScore,
		},
		{
			name: "single-tier router clamps floors to its only tier",
			mut: func(c *SmartRouterConfig) {
				c.Tiers = c.Tiers[:1]
			},
			req:      map[string]any{"messages": []any{msg("user", "hello")}, "reasoning_effort": "high", "tools": []any{map[string]any{}}},
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore,
		},
		{
			name: "openai image_url part sets hasImage",
			req: chatMsgs(msg("user", []any{
				part("text", "text", "what is in this picture"),
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
			})),
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore, wantImage: true,
		},
		{
			name: "anthropic image part sets hasImage",
			req: map[string]any{"messages": []any{msg("user", []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
				part("text", "text", "describe"),
			})}},
			endpoint: cacheEndpointMessages,
			wantRank: 0, wantWhy: smartReasonScore, wantImage: true,
		},
		{
			name: "image-only user turn has no text and uses the default tier",
			req: chatMsgs(msg("user", []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}},
			})),
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonDefault, wantImage: true,
		},
		{
			name: "image earlier in history still flags the request",
			req: chatMsgs(
				msg("user", []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}}, part("text", "text", "look")}),
				msg("assistant", "a cat"),
				msg("user", "and now?"),
			),
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore, wantImage: true,
		},
		{
			name: "malformed messages are skipped",
			req: map[string]any{"messages": []any{
				"not a map", nil, 42, map[string]any{"role": 7, "content": 7}, msg("user", "hello"),
			}},
			endpoint: cacheEndpointChat,
			wantRank: 0, wantWhy: smartReasonScore, wantText: "hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := newClassifyTestRouter(t, tt.mut)
			rank, why, text, img := classifySmartRequest(rt, tt.req, tt.endpoint)
			if rank != tt.wantRank || why != tt.wantWhy {
				t.Errorf("rank, reason = %d, %q; want %d, %q", rank, why, tt.wantRank, tt.wantWhy)
			}
			if img != tt.wantImage {
				t.Errorf("hasImage = %v, want %v", img, tt.wantImage)
			}
			if tt.wantText != "" && text != tt.wantText {
				t.Errorf("scoredText = %q, want %q", text, tt.wantText)
			}
		})
	}
}

func TestClassifySmartRequest_OnlyScansTheNewestWindow(t *testing.T) {
	t.Parallel()
	rt := newClassifyTestRouter(t, func(c *SmartRouterConfig) { c.DefaultTier = "mid" })
	msgs := []any{msg("user", strongAsk)}
	for i := 0; i < smartHistoryWindow; i++ {
		msgs = append(msgs, msg("assistant", "ok"))
	}
	rank, why, _, _ := classifySmartRequest(rt, map[string]any{"messages": msgs}, cacheEndpointChat)
	if rank != 1 || why != smartReasonDefault {
		t.Errorf("rank, reason = %d, %q; want the default tier because the only user turn is outside the window", rank, why)
	}
}

func TestClassifySmartRequest_BoundsScoredText(t *testing.T) {
	t.Parallel()
	rt := newClassifyTestRouter(t, func(c *SmartRouterConfig) {
		c.KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: []string{"needle"}}}
	})
	huge := strings.Repeat("a", 200000) + " needle"
	rank, why, text, _ := classifySmartRequest(rt, chatMsgs(msg("user", huge)), cacheEndpointChat)
	if len(text) > smartTextCap+1 {
		t.Errorf("scoredText length = %d, want at most %d", len(text), smartTextCap+1)
	}
	if rank != 2 || why != smartReasonKeyword {
		t.Errorf("rank, reason = %d, %q; want the keyword at the tail of a huge prompt to match", rank, why)
	}
}

func TestClassifySmartRequest_Deterministic(t *testing.T) {
	t.Parallel()
	rt := newClassifyTestRouter(t, nil)
	req := chatMsgs(msg("user", strongAsk))
	r1, w1, t1, i1 := classifySmartRequest(rt, req, cacheEndpointChat)
	r2, w2, t2, i2 := classifySmartRequest(rt, req, cacheEndpointChat)
	if r1 != r2 || w1 != w2 || t1 != t2 || i1 != i2 {
		t.Errorf("classification is not deterministic: (%d,%q,%q,%v) vs (%d,%q,%q,%v)", r1, w1, t1, i1, r2, w2, t2, i2)
	}
}

func TestClassifySmartRequest_WeightOverrides(t *testing.T) {
	t.Parallel()
	zero := 0
	boosted := 900
	req := chatMsgs(msg("user", "```go\nfunc f() {}\n```\ndebug this exception"))
	if rank, _, _, _ := classifySmartRequest(newClassifyTestRouter(t, func(c *SmartRouterConfig) {
		c.Weights = &SmartRouterWeights{Code: &zero}
	}), req, cacheEndpointChat); rank != 0 {
		t.Errorf("code weight 0: rank = %d, want 0", rank)
	}
	if rank, _, _, _ := classifySmartRequest(newClassifyTestRouter(t, func(c *SmartRouterConfig) {
		c.Weights = &SmartRouterWeights{Code: &boosted}
	}), req, cacheEndpointChat); rank != 2 {
		t.Errorf("code weight 900: rank = %d, want 2", rank)
	}
}

func BenchmarkClassifySmartRequest(b *testing.B) {
	rt := newClassifyTestRouter(b, func(c *SmartRouterConfig) {
		c.KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: []string{"gdpr", "hipaa", "soc2"}}}
	})
	body := strings.Repeat("Please review this service and explain how the request path handles retries. ", 60)
	req := map[string]any{
		"messages": []any{
			msg("system", "You are a helpful assistant."),
			msg("user", "Earlier question about deployment."),
			msg("assistant", "Sure, here is how it works."),
			msg("user", []any{part("text", "text", body+"```go\nfunc main() {}\n```")}),
		},
		"tools": []any{map[string]any{"type": "function"}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifySmartRequest(rt, req, cacheEndpointChat)
	}
}

func TestClassifySmartRequest_ImageScanCoversWholeHistoryAndToolResults(t *testing.T) {
	t.Parallel()
	rt := newClassifyTestRouter(t, nil)
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}}

	old := []any{msg("user", []any{img, part("text", "text", "look")})}
	for i := 0; i < smartHistoryWindow+5; i++ {
		old = append(old, msg("assistant", "ok"))
	}
	if _, _, _, got := classifySmartRequest(rt, map[string]any{"messages": old}, cacheEndpointChat); !got {
		t.Error("image older than the text window must still set hasImage")
	}

	toolResult := map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
		part("text", "text", "screenshot"),
		map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
	}}
	req := map[string]any{"messages": []any{msg("user", []any{toolResult})}}
	if _, _, _, got := classifySmartRequest(rt, req, cacheEndpointMessages); !got {
		t.Error("image inside tool_result content must set hasImage")
	}

	textOnly := map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "plain"}
	if _, _, _, got := classifySmartRequest(rt, map[string]any{"messages": []any{msg("user", []any{textOnly})}}, cacheEndpointMessages); got {
		t.Error("text-only tool_result must not set hasImage")
	}
}
