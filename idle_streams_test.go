package traefikllmgateway

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// idleSSEUpstream answers every request with one event, an idle gap longer
// than the gateway's watchdog, then a second event.
func idleSSEUpstream(t *testing.T, gap time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message\ndata: first\n\n"))
		fl.Flush()
		select {
		case <-time.After(gap):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("event: message\ndata: second\n\n"))
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newIdleStreamGateway builds a gateway with a 100ms target watchdog and one MCP target.
func newIdleStreamGateway(t *testing.T, targetURL string) *Gateway {
	t.Helper()
	cfg := CreateConfig()
	cfg.RequestTimeout = "100ms"
	cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}}}
	cfg.MCPServers = map[string]*TargetConfig{"notes": {URL: targetURL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h.(*Gateway)
}

// streamData serves one request through gw on a real listener and returns the data lines the client received.
func streamData(t *testing.T, gw *Gateway, method string, during func()) []string {
	t.Helper()
	front := httptest.NewServer(gw)
	t.Cleanup(front.Close)
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, front.URL+"/mcp/notes", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimPrefix(line, "data: "))
			if len(got) == 1 && during != nil {
				during()
			}
		}
	}
	return got
}

func TestTargetProxy_IdleStreamWatchdog(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   []string
	}{
		// An MCP GET event stream is the server-to-client channel and may idle for any time.
		{"mcp GET event stream outlives the watchdog", http.MethodGet, []string{"first", "second"}},
		// A POST response stream is a tool call in progress; a stall must still be cut.
		{"mcp POST event stream keeps the watchdog", http.MethodPost, []string{"first"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := newIdleStreamGateway(t, idleSSEUpstream(t, 400*time.Millisecond).URL)
			got := streamData(t, gw, tt.method, nil)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("received %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDrainIdleStreams(t *testing.T) {
	gw := newIdleStreamGateway(t, idleSSEUpstream(t, 30*time.Second).URL)
	start := time.Now()
	got := streamData(t, gw, http.MethodGet, gw.DrainIdleStreams)
	if len(got) != 1 || got[0] != "first" {
		t.Errorf("received %v, want only the event sent before the drain", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("drained stream took %v to end, want it ended promptly", d)
	}
	// A stream opened after the drain ends at once instead of idling.
	start = time.Now()
	got = streamData(t, gw, http.MethodGet, nil)
	if len(got) > 1 {
		t.Errorf("stream opened after the drain received %v, want at most the first event", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stream opened after the drain took %v, want it ended at once", d)
	}
}

func TestIdleStreamSetNilSafe(t *testing.T) {
	var s *idleStreamSet
	if _, ok := s.add(func() {}); !ok {
		t.Error("nil set refused a stream")
	}
	s.remove(1)
	s.drain()
}
