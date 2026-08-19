package traefikllmgateway

import (
	"fmt"
	"os"
)

// logf writes an info-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] INFO "+format+"\n", append([]any{g.name}, args...)...)
}

// errorf writes an error-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] ERROR "+format+"\n", append([]any{g.name}, args...)...)
}
