package traefikllmgateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("LLMGW_T", "from-env")
	dir := t.TempDir()
	fp := filepath.Join(dir, "k")
	if err := os.WriteFile(fp, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "plain", false},
		{"env:LLMGW_T", "from-env", false},
		{"env:LLMGW_MISSING", "", true},
		{"file:" + fp, "from-file", false},
		{"file:" + fp + ".nope", "", true},
		{"", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := resolveSecret(c.in)
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("resolveSecret(%q) = %q, %v; wantErr %v", c.in, got, err, c.wantErr)
			}
		})
	}
}
