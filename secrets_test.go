package traefikllmgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("LLMGW_T", "from-env")
	dir := t.TempDir()
	fp := filepath.Join(dir, "k")
	if err := os.WriteFile(fp, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	emptyFP := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFP, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	blankFP := filepath.Join(dir, "blank")
	if err := os.WriteFile(blankFP, []byte("   \n\t\n"), 0o600); err != nil {
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
		{"file:" + emptyFP, "", true},
		{"file:" + blankFP, "", true},
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

// TestResolveSecret_EmptyFile_ErrorNamesPathNotContents is the symmetry
// regression: env: and file: must both reject an empty resolved secret, and
// the error must name the path, never echo the (empty) contents.
func TestResolveSecret_EmptyFile_ErrorNamesPathNotContents(t *testing.T) {
	dir := t.TempDir()
	emptyFP := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFP, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := resolveSecret("file:" + emptyFP)
	if err == nil {
		t.Fatal("want error for empty file-resolved secret, got nil")
	}
	if !strings.Contains(err.Error(), emptyFP) {
		t.Fatalf("want error to name the path %q, got %q", emptyFP, err.Error())
	}
}

// TestResolveSecret_ErrorNeverLeaksSecretValue is a regression test: across
// env and file resolution, on both the success and the failure path, the
// returned error (when non-nil) must never contain the resolved secret
// value — only the env var name or file path, which are config, not secrets.
func TestResolveSecret_ErrorNeverLeaksSecretValue(t *testing.T) {
	const envSecret = "super-secret-env-value"
	const fileSecret = "super-secret-file-value"
	t.Setenv("LLMGW_LEAK_T", envSecret)

	dir := t.TempDir()
	fp := filepath.Join(dir, "leak")
	if err := os.WriteFile(fp, []byte(fileSecret+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	emptyFP := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFP, []byte(""), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name string
		in   string
	}{
		{"env success", "env:LLMGW_LEAK_T"},
		{"env failure (unset)", "env:LLMGW_LEAK_MISSING"},
		{"file success", "file:" + fp},
		{"file failure (empty)", "file:" + emptyFP},
		{"file failure (missing)", "file:" + fp + ".nope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveSecret(c.in)
			if err == nil {
				return // success path: nothing was resolved, nothing to leak
			}
			for _, secret := range []string{envSecret, fileSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks secret value: %v", err)
				}
			}
		})
	}
}
