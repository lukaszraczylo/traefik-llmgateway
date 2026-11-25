package traefikllmgateway

import (
	"fmt"
	"net/http"
	"strings"
)

// Request kinds the gateway classifies itself. Operators can name any other
// kind in a provider's Kinds map and list it in Config.LimitExemptKinds.
const (
	kindEmbedding = "embedding"
	kindReranking = "reranking"
)

// validateLimitExemptKinds rejects blank entries.
func validateLimitExemptKinds(kinds []string) error {
	for _, k := range kinds {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("llmgateway: limitExemptKinds must not contain a blank kind")
		}
	}
	return nil
}

// kindExempt reports whether requests of kind skip every limit.
func (g *Gateway) kindExempt(kind string) bool {
	if kind == "" {
		return false
	}
	for _, k := range g.cfg.LimitExemptKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// exemptScopes returns scopes without their limits, for a limit-exempt
// request: later steps (the unpriced-model cost-budget refusal, the minute
// token window) see no limit, while every usage counter keeps counting.
func exemptScopes(scopes []limitScope) []limitScope {
	out := make([]limitScope, len(scopes))
	for i, sc := range scopes {
		sc.limits = nil
		sc.noCount = true
		out[i] = sc
	}
	return out
}

// builtinPathKind classifies a passthrough path by its last segment.
func builtinPathKind(rest string) string {
	i := strings.LastIndex(rest, "/")
	switch rest[i+1:] {
	case "rerank", "reranking":
		return kindReranking
	case "embeddings":
		return kindEmbedding
	}
	return ""
}

// configuredPathKind returns the kind a provider's Kinds map assigns to a
// passthrough path: the exact key, else the longest key that is a path prefix.
func configuredPathKind(kinds map[string]string, rest string) string {
	if k, ok := kinds[rest]; ok {
		return k
	}
	best := ""
	kind := ""
	for key, k := range kinds {
		if !strings.HasPrefix(key, "/") || len(key) <= len(best) {
			continue
		}
		if strings.HasPrefix(rest, strings.TrimSuffix(key, "/")+"/") {
			best, kind = key, k
		}
	}
	return kind
}

// passthroughKind classifies a native passthrough request before admission:
// the provider's configured path kind, then the built-in path kind, then the
// kind of the model the body names. It does nothing when no kind is exempt,
// so the default path never peeks at the body. The body peek is bounded and
// restores r.Body.
func (g *Gateway) passthroughKind(r *http.Request, providerName, rest string) string {
	if len(g.cfg.LimitExemptKinds) == 0 {
		return ""
	}
	rest = "/" + strings.TrimPrefix(rest, "/")
	pc := g.cfg.Providers[providerName]
	if pc != nil {
		if k := configuredPathKind(pc.Kinds, rest); k != "" {
			return k
		}
	}
	if k := builtinPathKind(rest); k != "" {
		return k
	}
	if pc == nil || !g.hasExemptModelKind(pc.Kinds) {
		return ""
	}
	model, hasModel, truncated, err := peekPassthroughModel(r)
	if err != nil || truncated || !hasModel {
		return ""
	}
	if k, ok := pc.Kinds[model]; ok {
		return k
	}
	return pc.Kinds[strings.TrimPrefix(model, providerName+"/")]
}

// hasExemptModelKind reports whether kinds maps any model id (a key that is
// not a path) to an exempt kind.
func (g *Gateway) hasExemptModelKind(kinds map[string]string) bool {
	for key, k := range kinds {
		if !strings.HasPrefix(key, "/") && g.kindExempt(k) {
			return true
		}
	}
	return false
}
