package traefikllmgateway

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Yaegi unifies context key types of identical shape (see
// latencyRecorderCtxKey), so this one must differ in size from the others.
type upstreamModelCtxKey struct{ _ [2]byte }

// withUpstreamModel carries the resolved bare upstream model id to the
// adapter, so it can pick a per-model base URL. An empty model returns ctx
// unchanged.
func withUpstreamModel(ctx context.Context, model string) context.Context {
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, upstreamModelCtxKey{}, model)
}

func upstreamModelFromContext(ctx context.Context) string {
	model, _ := ctx.Value(upstreamModelCtxKey{}).(string)
	return model
}

// baseForCtx returns the base URL for the model ctx carries, else baseURL.
func (a *openaiAdapter) baseForCtx(ctx context.Context) string {
	if len(a.modelBases) == 0 {
		return a.baseURL
	}
	return a.baseForModel(upstreamModelFromContext(ctx))
}

func (a *openaiAdapter) baseForModel(model string) string {
	if b, ok := a.modelBases[model]; ok {
		return b
	}
	return a.baseURL
}

// hasUpstreamOverrides reports whether any per-model or per-path upstream is set.
func (a *openaiAdapter) hasUpstreamOverrides() bool {
	return len(a.modelBases) > 0 || len(a.pathBases) > 0
}

// passthroughBase picks the base URL for a native passthrough request:
// a model entry first, then the longest matching path prefix, else baseURL.
// path starts with "/"; model may be empty or carry the "<provider>/" prefix.
func (a *openaiAdapter) passthroughBase(model, path string) string {
	if model != "" {
		model = strings.TrimPrefix(model, a.adapterName+"/")
		if b, ok := a.modelBases[model]; ok {
			return b
		}
	}
	best := ""
	found := false
	for prefix := range a.pathBases {
		if !pathHasPrefix(path, prefix) {
			continue
		}
		if !found || len(prefix) > len(best) {
			best = prefix
			found = true
		}
	}
	if found {
		return a.pathBases[best]
	}
	return a.baseURL
}

// pathHasPrefix matches on whole path segments: "/v1/decide" covers
// "/v1/decide" and "/v1/decide/batch" but not "/v1/decider".
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

// validateUpstreamBases checks and normalizes a ModelBaseURLs or PathBaseURLs
// map. pathKeys requires each key to start with "/". Errors name the key
// only, never the URL, which may be malformed in a credential-bearing way.
func validateUpstreamBases(providerName, field string, in map[string]string, pathKeys bool) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for key, raw := range in {
		if key == "" {
			return nil, fmt.Errorf("llmgateway: provider %q: %s has an empty key", providerName, field)
		}
		if pathKeys && !strings.HasPrefix(key, "/") {
			return nil, fmt.Errorf("llmgateway: provider %q: %s key %q must start with \"/\"", providerName, field, key)
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("llmgateway: provider %q: %s[%q] must be an absolute http(s) URL", providerName, field, key)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("llmgateway: provider %q: %s[%q] must not carry userinfo, query or fragment", providerName, field, key)
		}
		out[key] = strings.TrimSuffix(raw, "/")
	}
	return out, nil
}
