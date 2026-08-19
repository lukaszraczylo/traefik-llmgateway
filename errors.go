package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
)

// writeOAIError writes an OpenAI-compatible error envelope:
// {"error":{"message":...,"type":...,"code":...}}.
func writeOAIError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": errType, "code": status},
	})
}

// recoverPanic recovers a panic from a handler, logs it, and writes a 500
// error envelope instead of letting the panic escape ServeHTTP.
func recoverPanic(w http.ResponseWriter, g *Gateway) {
	if rec := recover(); rec != nil {
		g.errorf("panic recovered: %v\n%s", rec, debug.Stack())
		writeOAIError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}
