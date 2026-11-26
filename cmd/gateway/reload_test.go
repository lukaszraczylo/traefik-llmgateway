package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	traefikllmgateway "github.com/lukaszraczylo/traefik-llmgateway"
)

// countingGen returns a generation over h whose close increments *closes.
func countingGen(h http.Handler, closes *atomic.Int32) *generation {
	return newGeneration(h, func() { closes.Add(1) })
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func TestGenerationLifecycle(t *testing.T) {
	tests := []struct {
		run        func(g *generation)
		name       string
		wantCloses int32
	}{
		{name: "idle retire closes at once", run: func(g *generation) { g.retire(time.Hour) }, wantCloses: 1},
		{name: "retire waits for the running request", run: func(g *generation) {
			g.enter()
			g.retire(time.Hour)
		}, wantCloses: 0},
		{name: "last leave closes a retired generation", run: func(g *generation) {
			g.enter()
			g.enter()
			g.retire(time.Hour)
			g.leave()
			g.leave()
		}, wantCloses: 1},
		{name: "leave on a live generation keeps it open", run: func(g *generation) {
			g.enter()
			g.leave()
		}, wantCloses: 0},
		{name: "enter after retire is refused and closes nothing twice", run: func(g *generation) {
			g.retire(time.Hour)
			if g.enter() {
				t.Error("enter on a retired generation must return false")
			}
		}, wantCloses: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var closes atomic.Int32
			g := countingGen(okHandler(), &closes)
			tt.run(g)
			if got := closes.Load(); got != tt.wantCloses {
				t.Errorf("closes = %d, want %d", got, tt.wantCloses)
			}
		})
	}
}

func TestGenerationDrainTimeoutForcesClose(t *testing.T) {
	var closes atomic.Int32
	g := countingGen(okHandler(), &closes)
	g.enter()
	g.retire(20 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if closes.Load() != 1 {
		t.Fatalf("closes = %d after the drain timeout, want 1", closes.Load())
	}
	g.leave()
	if closes.Load() != 1 {
		t.Errorf("a leave after the forced close closed again: %d", closes.Load())
	}
}

// TestSwapHandlerKeepsLongRequestOnOldGeneration: a request that started
// before a swap finishes on its own gateway, new requests reach the new
// one, and the old gateway closes only when the long request ends.
func TestSwapHandlerKeepsLongRequestOnOldGeneration(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	oldH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("X-Gen", "old")
	})
	newH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Gen", "new") })

	var oldCloses, newCloses atomic.Int32
	sw := &swapHandler{}
	sw.cur.Store(countingGen(oldH, &oldCloses))

	longRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		sw.ServeHTTP(longRec, httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	<-started

	sw.swap(countingGen(newH, &newCloses), time.Hour)

	rec := httptest.NewRecorder()
	sw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Gen"); got != "new" {
		t.Errorf("request after swap served by %q, want new", got)
	}
	if oldCloses.Load() != 0 {
		t.Fatal("old gateway closed while its request was still running")
	}
	close(release)
	<-done
	if got := longRec.Header().Get("X-Gen"); got != "old" {
		t.Errorf("long request served by %q, want old", got)
	}
	if oldCloses.Load() != 1 || newCloses.Load() != 0 {
		t.Errorf("closes old=%d new=%d, want 1 and 0", oldCloses.Load(), newCloses.Load())
	}
}

// TestSwapHandlerNeverServesOnClosedGeneration races requests against
// swaps under -race. Each handler fails the test if its own gateway is
// already closed when it runs: that is the invariant formal/Gateway/Reload.lean
// proves for the packed state word.
func TestSwapHandlerNeverServesOnClosedGeneration(t *testing.T) {
	sw := &swapHandler{}
	var bad atomic.Int32
	mk := func() *generation {
		var closed atomic.Bool
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if closed.Load() {
				bad.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		return newGeneration(h, func() { closed.Store(true) })
	}
	sw.cur.Store(mk())

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			}
		}()
	}
	for i := 0; i < 300; i++ {
		sw.swap(mk(), time.Hour)
	}
	close(stop)
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d requests ran on a closed generation", n)
	}
}

func TestCloseCurrentTurnsAwayLaterRequests(t *testing.T) {
	var closes atomic.Int32
	sw := &swapHandler{}
	g := countingGen(okHandler(), &closes)
	sw.cur.Store(g)
	sw.closeCurrent()
	if closes.Load() != 1 {
		t.Fatalf("closes = %d, want 1", closes.Load())
	}
	if g.enter() {
		t.Error("enter after closeCurrent must return false")
	}
}

func TestShutStopsDrainTimer(t *testing.T) {
	var closes atomic.Int32
	g := countingGen(okHandler(), &closes)
	g.enter()
	g.retire(time.Hour)
	g.leave()
	tm := g.timer.Load()
	if tm == nil {
		t.Fatal("retire with a running request set no drain timer")
	}
	if tm.Stop() {
		t.Error("drain timer still pending after the generation closed")
	}
}

func TestParseOptionsReloadBounds(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"defaults", nil, false},
		{"polling off", []string{"-reload-interval", "0"}, false},
		{"negative interval", []string{"-reload-interval", "-1s"}, true},
		{"zero drain", []string{"-reload-drain-timeout", "0"}, true},
		{"negative drain", []string{"-reload-drain-timeout", "-1s"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseOptions(tt.args, os.Stderr)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// fakeBuild records each build and fails when the file holds "bad".
type fakeBuild struct {
	calls atomic.Int32
}

func (f *fakeBuild) build(raw []byte) (http.Handler, error) {
	f.calls.Add(1)
	if strings.Contains(string(raw), "bad") {
		return nil, errors.New("rejected")
	}
	body := string(raw)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), nil
}

func serveBody(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

func TestReloaderAppliesOnlyChangedValidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	fb := &fakeBuild{}
	h0, _ := fb.build([]byte("one"))
	sw := &swapHandler{}
	sw.cur.Store(newGeneration(h0, func() {}))
	r := newReloader(path, []byte("one"), fb.build, sw, time.Hour, newLogger("error"))
	fb.calls.Store(0)

	steps := []struct {
		name      string
		content   string
		wantBody  string
		wantCalls int32
		force     bool
	}{
		{"same content does not rebuild", "one", "one", 0, false},
		{"changed content applies", "two", "two", 1, false},
		{"rejected config keeps the old gateway", "bad", "two", 2, false},
		{"rejected content is retried on the next check", "bad", "two", 3, false},
		{"a fixed file applies", "three", "three", 4, false},
		{"applied content is not rebuilt again", "three", "three", 4, false},
		{"force rebuilds unchanged content", "three", "three", 5, true},
	}
	for _, s := range steps {
		write(s.content)
		r.check(s.force)
		if got := serveBody(t, sw); got != s.wantBody {
			t.Errorf("%s: served %q, want %q", s.name, got, s.wantBody)
		}
		if got := fb.calls.Load(); got != s.wantCalls {
			t.Errorf("%s: builds = %d, want %d", s.name, got, s.wantCalls)
		}
	}
}

func TestReloaderMissingFileKeepsServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	fb := &fakeBuild{}
	h0, _ := fb.build([]byte("one"))
	sw := &swapHandler{}
	sw.cur.Store(newGeneration(h0, func() {}))
	r := newReloader(path, []byte("one"), fb.build, sw, time.Hour, newLogger("error"))
	r.check(false)
	if got := serveBody(t, sw); got != "one" {
		t.Errorf("served %q after the file vanished, want one", got)
	}
}

// TestReloadRealGateway reloads a real gateway from a config file and
// checks that the model list follows the file.
func TestReloadRealGateway(t *testing.T) {
	const tmpl = `providers:
  openai:
    type: openai
    apiKey: test-key
    models:
      - %s
groups:
  default:
    providers: []
users:
  inline:
    - name: tester
      group: default
      apiKey: test-user-key
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfgFor := func(model string) []byte { return []byte(strings.Replace(tmpl, "%s", model, 1)) }
	if err := os.WriteFile(path, cfgFor("model-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := newLogger("error")
	build := gatewayBuilder(context.Background(), path, "test", log)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sw := &swapHandler{}
	sw.cur.Store(newGeneration(h, func() { closeGateway(h, log) }))
	r := newReloader(path, raw, build, sw, time.Hour, log)
	t.Cleanup(sw.closeCurrent)

	models := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-user-key")
		sw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/v1/models = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := models(); !strings.Contains(body, "model-a") {
		t.Fatalf("initial models lack model-a: %s", body)
	}
	if err := os.WriteFile(path, cfgFor("model-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.check(false)
	body := models()
	if !strings.Contains(body, "model-b") || strings.Contains(body, "model-a") {
		t.Fatalf("models after reload = %s, want model-b only", body)
	}
}

// TestGatewayBuilderRejectsUnknownField keeps the strict decode on reload.
func TestGatewayBuilderRejectsUnknownField(t *testing.T) {
	build := gatewayBuilder(context.Background(), "config.yaml", "test", newLogger("error"))
	if _, err := build([]byte("providers: {}\nnotAField: 1\n")); err == nil {
		t.Fatal("unknown field accepted on reload")
	}
}

var _ http.Handler = (*traefikllmgateway.Gateway)(nil)
