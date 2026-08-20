package traefikllmgateway

import (
	"encoding/json"
	"net/http"
)

// handleModels implements GET /v1/models: an authenticated caller gets back
// an OpenAI-compatible {"object":"list","data":[...]} envelope of the
// models their group can see, via modelRegistry.listFor.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	_, grp, ok := g.auth.identify(r)
	if !ok {
		writeOAIError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"object": "list",
		"data":   g.registry.listFor(grp),
	})
}
