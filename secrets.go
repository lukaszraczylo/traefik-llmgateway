package traefikllmgateway

import (
	"fmt"
	"os"
	"strings"
)

// resolveSecret resolves a config string value to its underlying secret. An
// empty value returns empty with no error. A "env:NAME" value returns
// os.Getenv(NAME), erroring if the variable is unset or empty. A
// "file:/path" value returns the file's contents with surrounding
// whitespace trimmed, erroring if the file cannot be read. Any other value
// is returned unchanged as a literal.
//
// Error messages never include a resolved secret value — only the env var
// name or file path, which are config, not secrets.
func resolveSecret(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if name, ok := strings.CutPrefix(v, "env:"); ok {
		val := os.Getenv(name)
		if val == "" {
			return "", fmt.Errorf("llmgateway: env var %q is unset or empty", name)
		}
		return val, nil
	}
	if path, ok := strings.CutPrefix(v, "file:"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("llmgateway: cannot read secret file %q: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}
