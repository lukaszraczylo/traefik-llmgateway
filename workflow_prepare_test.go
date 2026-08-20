package traefikllmgateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// copyWorkflowPrepareFixture copies the repository's workflow-prepare.sh and
// version.go into a fresh temp directory and returns that directory. The
// script hardcodes "version.go" relative to its own working directory (see
// workflow-prepare.sh's FILE var), so the test runs it against a throwaway
// copy — never against the tracked version.go, which must keep carrying the
// "0.0.0-dev" sentinel in this package's own source.
func copyWorkflowPrepareFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range []string{"workflow-prepare.sh", "version.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o600); err != nil { //nolint:gosec // G703: name ranges over a fixed literal slice above, not external input; dir is t.TempDir()
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return dir
}

// runWorkflowPrepare runs `bash workflow-prepare.sh` with dir as the working
// directory (the fixture from copyWorkflowPrepareFixture) and env appended
// as extra VAR=value entries. Go's exec.Cmd documents that a later duplicate
// key in Env wins, so entries here override any same-named var inherited
// from the test process.
func runWorkflowPrepare(t *testing.T, dir string, env ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command("bash", "workflow-prepare.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	err := cmd.Run()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run workflow-prepare.sh: %v", err)
		}
		return out.String(), errOut.String(), exitErr.ExitCode()
	}

	return out.String(), errOut.String(), 0
}

func readFixtureVersion(t *testing.T, dir string) string {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(dir, "version.go"))
	if err != nil {
		t.Fatalf("read stamped version.go: %v", err)
	}
	return string(got)
}

func TestWorkflowPrepare(t *testing.T) {
	t.Parallel()

	t.Run("good: stamps VERSION into the const", func(t *testing.T) {
		t.Parallel()
		dir := copyWorkflowPrepareFixture(t)

		stdout, stderr, code := runWorkflowPrepare(t, dir, "VERSION=9.9.9")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stdout=%s stderr=%s", code, stdout, stderr)
		}

		got := readFixtureVersion(t, dir)
		if !strings.Contains(got, `const pluginVersion = "9.9.9"`) {
			t.Errorf("version.go does not contain the stamped const, got:\n%s", got)
		}
		if strings.Contains(got, `const pluginVersion = "0.0.0-dev"`) {
			t.Errorf("version.go const still carries the dev sentinel after stamping:\n%s", got)
		}
	})

	t.Run("good: strips a leading v from VERSION", func(t *testing.T) {
		t.Parallel()
		dir := copyWorkflowPrepareFixture(t)

		_, stderr, code := runWorkflowPrepare(t, dir, "VERSION=v1.2.3")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
		}

		got := readFixtureVersion(t, dir)
		if !strings.Contains(got, `pluginVersion = "1.2.3"`) {
			t.Errorf("leading 'v' not stripped, got:\n%s", got)
		}
	})

	t.Run("edge: no version env leaves the sentinel untouched", func(t *testing.T) {
		t.Parallel()
		dir := copyWorkflowPrepareFixture(t)

		_, _, code := runWorkflowPrepare(t, dir,
			"VERSION=", "VERSION_TAG=", "SEMVER=", "NEW_VERSION=", "RELEASE_VERSION=", "GITHUB_ACTIONS=")
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (no-op) when no version env is set", code)
		}

		got := readFixtureVersion(t, dir)
		if !strings.Contains(got, `pluginVersion = "0.0.0-dev"`) {
			t.Errorf("version.go mutated on a no-op run, got:\n%s", got)
		}
	})

	t.Run("bad: malformed VERSION exits non-zero and leaves the file untouched", func(t *testing.T) {
		t.Parallel()
		dir := copyWorkflowPrepareFixture(t)

		_, stderr, code := runWorkflowPrepare(t, dir, "VERSION=not-a-version")
		if code == 0 {
			t.Fatalf("exit = 0, want non-zero for malformed VERSION")
		}
		if !strings.Contains(stderr, "not semver-shaped") {
			t.Errorf("stderr missing the semver error, got: %s", stderr)
		}

		got := readFixtureVersion(t, dir)
		if !strings.Contains(got, `pluginVersion = "0.0.0-dev"`) {
			t.Errorf("version.go was mutated despite the malformed VERSION, got:\n%s", got)
		}
	})
}
