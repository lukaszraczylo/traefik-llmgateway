package traefikllmgateway

import (
	"fmt"
	"os"
)

// logf writes an info-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] INFO %s\n", g.name, fmt.Sprintf(format, args...))
}

// errorf writes an error-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] ERROR %s\n", g.name, fmt.Sprintf(format, args...))
}
