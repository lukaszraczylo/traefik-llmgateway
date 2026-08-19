package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strconv"
)

// statusTrackingWriter wraps an http.ResponseWriter to record whether a
// response has already been committed (header written, or body bytes
// written under the net/http implicit-200 rule). recoverPanic uses this to
// avoid writing a second, conflicting response after a handler panics
// mid-stream. Flush delegates to the underlying http.Flusher when present —
// later tasks' SSE streaming depends on Flush passing through.
type statusTrackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

// WriteHeader records that the response was committed, then delegates.
func (w *statusTrackingWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

// Write records an implicit header commit if none happened yet, then
// delegates.
func (w *statusTrackingWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Flush delegates to the underlying ResponseWriter's Flush when it
// implements http.Flusher, and is a no-op otherwise.
func (w *statusTrackingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// writeOAIError writes an OpenAI-compatible error envelope:
// {"error":{"message":...,"type":...,"code":...}}. code is a JSON string —
// OpenAI-compatible SDKs expect string-or-null, not an integer.
func writeOAIError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": errType, "code": strconv.Itoa(status)},
	})
}

// recoverPanic recovers a panic from a handler and logs it. It writes a 500
// error envelope only if the response has not already been committed — a
// handler that panics after writing headers or body bytes (mid-stream) must
// not get a second, conflicting response appended.
func recoverPanic(w *statusTrackingWriter, g *Gateway) {
	if rec := recover(); rec != nil {
		g.errorf("panic recovered: %v\n%s", rec, debug.Stack())
		if w.wroteHeader {
			return
		}
		writeOAIError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}
