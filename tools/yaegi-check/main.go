// Command yaegi-check replicates the Traefik Plugin Catalog analyzer
// (piceus)'s yaegiMiddlewareCheck for this plugin. It loads the plugin
// package under Yaegi exactly as the catalog and Traefik's own plugin
// loader do: interpret the source tree, evaluate CreateConfig(), decode
// the plugin's real .traefik.yml testData into the resulting Config
// value, then call the plugin's New constructor by reflection from
// outside the interpreter. It then exercises the returned handler with
// two real HTTP requests, proving ServeHTTP, auth, the model registry,
// statusTrackingWriter, and the gateway's logger all run under Yaegi —
// not merely that the package's imports resolve.
//
// Any failure — a Yaegi-incompatible construct in the plugin's stdlib
// usage, a constructor error, a nil handler, or a wrong response from the
// live handler — exits non-zero: this is the gate the plugin must pass
// before it can list in the Traefik Plugin Catalog.
//
// This module is intentionally separate from the plugin module
// (llmgw-yaegi-check, not github.com/lukaszraczylo/traefik-llmgateway):
// the plugin module must stay free of any non-stdlib dependency, since
// Yaegi interprets it with only the Go standard library available: a
// third-party import in the plugin module's own go.mod would break under
// interpretation even though `go build` never notices.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// testDataUserAPIKey and testDataWantModel are drawn directly from the
// plugin's own .traefik.yml testData block (users.inline[0].apiKey and
// providers.openai.models[0]) — see exerciseHandler.
//
// attemptAccountingAdminAPIKey names the second, admin-flagged user run()
// layers on top of testData (see its own override comment) — SHOULD-4
// (v0.22 review round): this harness proves recordProviderAttempt's
// SHOULD-1 deadline/cancel classification (limits.go's matchesSentinel/
// isDeadlineExceeded, a hand-rolled errors.Unwrap walk specifically
// because errors.Is/errors.As are unsafe under Yaegi — see their own doc
// comments) actually runs correctly INTERPRETED, not merely compiled: a
// bug there would not show up under `go test`, only here.
const (
	testDataUserAPIKey           = "test-user-key"
	testDataWantModel            = "gpt-test"
	attemptAccountingAdminAPIKey = "sk-admin1"
)

// excludedTopLevelDirs lists repo-root directories the GOPATH copy must
// never include: build tooling, integration fixtures, planning docs, VCS
// metadata, and vendor (task-8's test-only testify dependency — _test.go
// files are never interpreted by Yaegi, so vendor/ has nothing this check
// needs, and copying its "go.yaml.in"-style nested module dirs into a
// module-less GOPATH tree would otherwise confuse Yaegi's own import
// resolution) have nothing to do with the plugin package Yaegi imports.
// webui/ (Vue admin-panel task) is excluded for the same reason vendor/
// is: its own go.mod already walls it off from the repo root's `go
// build`/`vet`/`test ./...`, and its node_modules tree — hundreds of MB,
// including a stray bundled .go file
// (node_modules/flatted/golang/pkg/flatted.go) — would otherwise both
// balloon copyRepoSource's wall time and risk Yaegi tripping over code
// this repo neither wrote nor can build. The plugin only ever imports the
// generated admin_assets_gen.go at the repo root, never anything under
// webui/ itself.
var excludedTopLevelDirs = map[string]bool{
	"tools":        true,
	"integration":  true,
	".superpowers": true,
	"docs":         true,
	".git":         true,
	"vendor":       true,
	"webui":        true,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "yaegi-check: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("yaegi-check: OK")
}

// run resolves the plugin's repo root (os.Args[1], defaulting to the
// current directory), builds a temporary GOPATH containing a copy of its
// source, interprets the plugin package under Yaegi, and exercises the
// constructed handler. It returns the first error encountered, wrapped
// with enough context to diagnose which stage failed.
func run() error {
	repoRoot := "."
	if len(os.Args) > 1 {
		repoRoot = os.Args[1]
	}
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}

	modulePath, err := readModulePath(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	gopath, err := os.MkdirTemp("", "llmgw-yaegi-check-*")
	if err != nil {
		return fmt.Errorf("create temp GOPATH: %w", err)
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // best-effort cleanup of a temp dir

	pkgDir := filepath.Join(gopath, "src", modulePath)
	if err = copyRepoSource(repoRoot, pkgDir); err != nil {
		return fmt.Errorf("copy repo source into GOPATH: %w", err)
	}

	pkgName, err := readPackageName(pkgDir)
	if err != nil {
		return fmt.Errorf("read plugin package name: %w", err)
	}

	i := interp.New(interp.Options{GoPath: gopath})
	if err = i.Use(stdlib.Symbols); err != nil {
		return fmt.Errorf("interp.Use(stdlib.Symbols): %w", err)
	}
	if _, err = i.Eval(`import "` + modulePath + `"`); err != nil {
		return fmt.Errorf("import plugin package under yaegi: %w", err)
	}

	cfgVal, err := i.Eval(pkgName + ".CreateConfig()")
	if err != nil {
		return fmt.Errorf("call %s.CreateConfig() under yaegi: %w", pkgName, err)
	}
	if !cfgVal.CanInterface() {
		return fmt.Errorf("%s.CreateConfig() result cannot be used via reflect.Value.Interface", pkgName)
	}

	testData, err := extractTraefikYAMLTestData(filepath.Join(repoRoot, ".traefik.yml"))
	if err != nil {
		return fmt.Errorf("extract .traefik.yml testData: %w", err)
	}
	testDataJSON, err := json.Marshal(testData)
	if err != nil {
		return fmt.Errorf("marshal testData: %w", err)
	}
	if err = json.Unmarshal(testDataJSON, cfgVal.Interface()); err != nil {
		return fmt.Errorf("decode .traefik.yml testData into the interpreted Config: %w", err)
	}

	// Feature A (v0.22) attempt-accounting harness (SHOULD-4, review
	// round): a second json.Unmarshal into the same cfgVal layers this
	// override on top of testData's own decode, rather than replacing it
	// outright — encoding/json's own map/struct-merge semantics keep
	// every testData field this override does not mention (Groups.default
	// in particular, which .traefik.yml's own testData.groups block
	// already provides and this override never touches). It replaces
	// providers.openai wholesale (a real map key, not merged field-by-
	// field) with one pointed at a local httptest upstream so the harness
	// can drive a real chat completion without a live provider, and adds
	// a second, admin-flagged user (Users.Inline is a slice — fully
	// replaced, not merged, which is why the "tester" user from testData
	// is respecified here too, identical apiKey/group, rather than
	// silently dropped).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	attemptAccountingOverride := `{"providers":{"openai":{"type":"openai","baseUrl":"` + upstream.URL + `","apiKey":"sk-up","models":["` + testDataWantModel + `"]}},"admin":{"enabled":true},"users":{"inline":[{"name":"tester","group":"default","apiKey":"` + testDataUserAPIKey + `"},{"name":"admin1","group":"default","apiKey":"` + attemptAccountingAdminAPIKey + `","admin":true}]}}`
	if err = json.Unmarshal([]byte(attemptAccountingOverride), cfgVal.Interface()); err != nil {
		return fmt.Errorf("decode attempt-accounting harness override into the interpreted Config: %w", err)
	}

	newVal, err := i.Eval(pkgName + ".New")
	if err != nil {
		return fmt.Errorf("resolve %s.New under yaegi: %w", pkgName, err)
	}
	if newVal.Kind() != reflect.Func {
		return fmt.Errorf("%s.New is not a function (kind %s)", pkgName, newVal.Kind())
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	results := newVal.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(next),
		cfgVal,
		reflect.ValueOf("llmgw-check"),
	})
	if len(results) != 2 {
		return fmt.Errorf("%s.New returned %d values, want 2 (http.Handler, error)", pkgName, len(results))
	}
	if errVal := results[1]; !errVal.IsNil() {
		return fmt.Errorf("%s.New returned an error: %v", pkgName, errVal.Interface())
	}
	handlerVal := results[0]
	if handlerVal.IsNil() {
		return fmt.Errorf("%s.New returned a nil handler", pkgName)
	}
	handler, ok := handlerVal.Interface().(http.Handler)
	if !ok {
		return fmt.Errorf("%s.New's returned value does not implement http.Handler", pkgName)
	}

	return exerciseHandler(handler)
}

// exerciseHandler runs two real requests against the interpreted
// handler, drawn from the plugin's own .traefik.yml testData: an
// authenticated GET /v1/models, which must return the configured
// testDataWantModel; and the same route without credentials, which must
// be refused with 401 — proving ServeHTTP, auth.identify, the model
// registry, statusTrackingWriter, and the gateway's logger all execute
// for real under Yaegi, not merely that the package's imports resolve.
func exerciseHandler(handler http.Handler) error {
	authedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authedReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	authedRec := httptest.NewRecorder()
	handler.ServeHTTP(authedRec, authedReq)
	if authedRec.Code != http.StatusOK {
		return fmt.Errorf("GET /v1/models with the testData user key: status = %d, want 200, body=%s", authedRec.Code, authedRec.Body.String())
	}
	if !strings.Contains(authedRec.Body.String(), testDataWantModel) {
		return fmt.Errorf("GET /v1/models body does not contain %q: %s", testDataWantModel, authedRec.Body.String())
	}

	if err := exerciseAttemptAccounting(handler); err != nil {
		return err
	}

	unauthedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	unauthedRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthedRec, unauthedReq)
	if unauthedRec.Code != http.StatusUnauthorized {
		return fmt.Errorf("GET /v1/models without a key: status = %d, want 401, body=%s", unauthedRec.Code, unauthedRec.Body.String())
	}
	return nil
}

// exerciseAttemptAccounting drives a real chat completion through
// handler (against the local httptest upstream run's own override wired
// in), then GET /admin/api/overview, asserting the resulting provider
// reports exactly one attempt and zero failures — Feature A's (v0.22)
// per-provider success-rate accounting, exercised end to end under
// Yaegi: runUnified's attemptRecorder (routes_unified.go) ->
// retryPolicy.do's context lookup (retry.go) ->
// limiter.recordProviderAttempt's isTransient/isDeadlineExceeded
// classification (limits.go) -> its fire-and-forget spawn (SHOULD-5) ->
// buildAdminOverview's own batched providerUsage read (admin.go). Any
// interpreter-only failure in that chain — a construct `go build`/`go
// test` cannot catch, exactly the class of bug this harness exists for
// (SHOULD-4, v0.22 review round) — surfaces here as a non-1/non-0 count
// or an outright panic under Yaegi.
func exerciseAttemptAccounting(handler http.Handler) error {
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+testDataWantModel+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	chatReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (attempt-accounting harness): status = %d, want 200, body=%s", chatRec.Code, chatRec.Body.String())
	}

	attemptsDay, failuresDay, err := pollProviderAttemptCounters(handler)
	if err != nil {
		return err
	}
	if attemptsDay != 1 {
		return fmt.Errorf("admin overview providers[0].attemptsDay = %v, want 1 (Feature A attempt-accounting harness)", attemptsDay)
	}
	if failuresDay != 0 {
		return fmt.Errorf("admin overview providers[0].failuresDay = %v, want 0", failuresDay)
	}
	return nil
}

// pollProviderAttemptCounters drives GET /admin/api/overview against
// handler repeatedly until providers[0].attemptsDay is non-zero or a 2s
// budget elapses, then returns its final reading either way — SHOULD-5
// (v0.22 review round): recordProviderAttempt's store write runs on its
// own goroutine (limiter.spawn), off the chat request that triggered it,
// so a single immediate read right after that request returns could race
// ahead of the write landing. A real goroutine still schedules promptly
// under Yaegi (only the INTERPRETED code driving it runs slower, not the
// underlying Go runtime's scheduler), so this is a short poll, not a
// long one.
func pollProviderAttemptCounters(handler http.Handler) (attemptsDay, failuresDay float64, err error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		attemptsDay, failuresDay, err = readProviderAttemptCounters(handler)
		if err != nil {
			return 0, 0, err
		}
		if attemptsDay != 0 || time.Now().After(deadline) {
			return attemptsDay, failuresDay, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readProviderAttemptCounters reads GET /admin/api/overview's first
// provider's attemptsDay/failuresDay fields via a generic map[string]any
// decode — this harness module is compiled, not interpreted, so it could
// import the plugin's own admin.go types directly, but they are
// unexported (adminOverviewResponse, adminProviderView); decoding
// generically here is simpler than exporting test-only types across that
// boundary just for this one harness.
func readProviderAttemptCounters(handler http.Handler) (attemptsDay, failuresDay float64, err error) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return 0, 0, fmt.Errorf("GET /admin/api/overview: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return 0, 0, fmt.Errorf("decode GET /admin/api/overview body: %w", err)
	}
	providers, ok := body["providers"].([]any)
	if !ok || len(providers) == 0 {
		return 0, 0, fmt.Errorf("admin overview body has no providers: %s", rec.Body.String())
	}
	p, ok := providers[0].(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf("admin overview providers[0] is not an object: %s", rec.Body.String())
	}
	attemptsDay, _ = p["attemptsDay"].(float64)
	failuresDay, _ = p["failuresDay"].(float64)
	return attemptsDay, failuresDay, nil
}

// readModulePath returns the module path declared by goModPath's
// "module" directive.
func readModulePath(goModPath string) (string, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no module directive found in %s", goModPath)
}

// readPackageName returns the package name declared by the first
// non-test .go file it finds directly inside dir. The plugin is a single
// root package, so any one of its files' declarations suffices.
func readPackageName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		pkgName, err := scanPackageDecl(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if pkgName != "" {
			return pkgName, nil
		}
	}
	return "", fmt.Errorf("no package declaration found in %s", dir)
}

// scanPackageDecl reads path's leading "package X" line, if any.
func scanPackageDecl(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "package "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", scanner.Err()
}

// copyRepoSource copies repoRoot into destDir, skipping every directory
// named in excludedTopLevelDirs. destDir plays the role of
// GOPATH/src/{modulePath} for the interpreter: Yaegi's source loader
// resolves the plugin's own import path by walking GOPATH exactly as the
// standard toolchain would for a pre-modules GOPATH-mode build.
func copyRepoSource(repoRoot, destDir string) error {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return err
	}
	for _, e := range entries {
		if excludedTopLevelDirs[e.Name()] {
			continue
		}
		src := filepath.Join(repoRoot, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if e.IsDir() {
			if err := copyDir(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target)
	})
}

// copyFile copies src to dst, creating dst's parent directory if needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // Close's error is checked explicitly below
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// yamlLine is one non-blank, non-comment line of a YAML file, with
// leading whitespace stripped and recorded as indent.
type yamlLine struct {
	text   string
	indent int
}

// extractTraefikYAMLTestData reads path (the plugin's .traefik.yml) and
// decodes its top-level "testData:" block into a generic
// map[string]any/[]any/scalar tree, using a small hand-rolled,
// indentation-based YAML subset parser: just enough for this one file's
// shape (nested maps, lists of scalars, lists of flat maps, "{}"/"[]"
// empty collections), not a general YAML implementation. It exists only
// in this harness module — the plugin module never parses YAML itself,
// Traefik does that before handing the plugin its already-decoded Config.
func extractTraefikYAMLTestData(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := tokenizeYAMLLines(string(raw))
	block := extractTestDataBlock(lines)
	if block == nil {
		return nil, fmt.Errorf("%s has no top-level testData: block", path)
	}
	pos := 0
	v := parseBlock(block, &pos, block[0].indent)
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s testData: block is not a map", path)
	}
	return m, nil
}

// tokenizeYAMLLines splits raw into non-blank, non-comment yamlLines,
// each carrying its own leading-space count as indent.
func tokenizeYAMLLines(raw string) []yamlLine {
	var out []yamlLine
	for _, ln := range strings.Split(raw, "\n") {
		trimmedRight := strings.TrimRight(ln, " \t\r")
		content := strings.TrimLeft(trimmedRight, " ")
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		out = append(out, yamlLine{indent: len(trimmedRight) - len(content), text: content})
	}
	return out
}

// extractTestDataBlock returns the lines nested under a top-level (indent
// 0) "testData:" line, or nil if lines has none.
func extractTestDataBlock(lines []yamlLine) []yamlLine {
	for i, l := range lines {
		if l.indent != 0 || l.text != "testData:" {
			continue
		}
		var block []yamlLine
		for j := i + 1; j < len(lines) && lines[j].indent > 0; j++ {
			block = append(block, lines[j])
		}
		return block
	}
	return nil
}

// parseBlock parses the run of lines starting at *pos that share indent,
// dispatching to parseList when the first such line is a "-" item and to
// parseMap otherwise.
func parseBlock(lines []yamlLine, pos *int, indent int) any {
	if *pos >= len(lines) || lines[*pos].indent != indent {
		return map[string]any{}
	}
	if strings.HasPrefix(lines[*pos].text, "-") {
		return parseList(lines, pos, indent)
	}
	return parseMap(lines, pos, indent)
}

// parseMap consumes every consecutive "key: value" line at indent,
// recursing into parseBlock for a key whose value is empty (a nested
// block follows on deeper-indented lines).
func parseMap(lines []yamlLine, pos *int, indent int) map[string]any {
	m := map[string]any{}
	for *pos < len(lines) && lines[*pos].indent == indent {
		key, val, _ := strings.Cut(lines[*pos].text, ":")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		*pos++
		if val != "" {
			m[key] = collectionOrScalar(val)
			continue
		}
		if *pos < len(lines) && lines[*pos].indent > indent {
			m[key] = parseBlock(lines, pos, lines[*pos].indent)
		} else {
			m[key] = map[string]any{}
		}
	}
	return m
}

// parseList consumes every consecutive "- ..." line at indent. A "-"
// item containing a colon starts a flat map (its own "key: value" plus
// every following line indented at least indent+2, until a line back at
// indent or shallower); any other item is a bare scalar.
func parseList(lines []yamlLine, pos *int, indent int) []any {
	var out []any
	for *pos < len(lines) && lines[*pos].indent == indent && strings.HasPrefix(lines[*pos].text, "-") {
		item := strings.TrimSpace(strings.TrimPrefix(lines[*pos].text, "-"))
		if item == "" {
			*pos++
			continue
		}
		if !strings.Contains(item, ":") {
			out = append(out, parseScalar(item))
			*pos++
			continue
		}

		m := map[string]any{}
		key, val, _ := strings.Cut(item, ":")
		m[strings.TrimSpace(key)] = collectionOrScalar(strings.TrimSpace(val))
		*pos++

		childIndent := indent + 2
		for *pos < len(lines) && lines[*pos].indent >= childIndent {
			k, v, _ := strings.Cut(lines[*pos].text, ":")
			m[strings.TrimSpace(k)] = collectionOrScalar(strings.TrimSpace(v))
			*pos++
		}
		out = append(out, m)
	}
	return out
}

// collectionOrScalar maps a "key: value" line's already-trimmed val to
// an empty map/slice for "{}"/"[]", or to parseScalar(val) otherwise. It
// does not recurse into a further nested block — no key in this file's
// testData needs a map/list value inside a list item's own fields, and
// this harness parses only what .traefik.yml actually contains.
func collectionOrScalar(val string) any {
	switch val {
	case "{}":
		return map[string]any{}
	case "[]":
		return []any{}
	default:
		return parseScalar(val)
	}
}

// parseScalar converts a YAML scalar token to a bool, int64, float64, or
// string, in that preference order, stripping a matching pair of quotes
// first.
func parseScalar(s string) any {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
