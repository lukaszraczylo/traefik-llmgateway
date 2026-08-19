package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCreateConfig_ReturnsEmptyConfig(t *testing.T) {
	cfg := CreateConfig()
	if cfg == nil {
		t.Fatal("CreateConfig returned nil")
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("want zero providers, got %d", len(cfg.Providers))
	}
}

func TestNew_NoProviders_ReturnsError(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, CreateConfig(), "llmgw")
	if err == nil {
		t.Fatal("want error for config with no providers, got nil")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

func TestNew_NilConfig_ReturnsError(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, nil, "llmgw")
	if err == nil {
		t.Fatal("want error for nil config, got nil")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

func TestGateway_PassthroughUnknown_ForwardsToNext(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.PassthroughUnknown = true
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("want next handler called when PassthroughUnknown is true")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("want %d from next handler, got %d", http.StatusTeapot, rec.Code)
	}
}

func TestWriteOAIError_WritesEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOAIError(rec, http.StatusBadRequest, "invalid_request_error", "bad request")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want status %d, got %d", http.StatusBadRequest, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %q", ct)
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "bad request" {
		t.Fatalf("want message %q, got %q", "bad request", body.Error.Message)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("want type %q, got %q", "invalid_request_error", body.Error.Type)
	}
	if body.Error.Code != http.StatusBadRequest {
		t.Fatalf("want code %d, got %d", http.StatusBadRequest, body.Error.Code)
	}
}

func TestServeHTTP_PanicRecovery_Returns500Envelope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	rec := httptest.NewRecorder()
	func() {
		defer recoverPanic(rec, gw)
		panic("boom")
	}()

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want %d, got %d", http.StatusInternalServerError, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %q", ct)
	}
	if !strings.Contains(buf.String(), "llmgw[llmgw] ERROR ") {
		t.Fatalf("want stderr to contain error log prefix, got %q", buf.String())
	}
}

func TestGateway_Logf_WritesInfoPrefix(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "mygw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	gw.logf("hello %s", "world")

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	want := "llmgw[mygw] INFO hello world"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("want stderr to contain %q, got %q", want, buf.String())
	}
}

func TestGateway_Errorf_WritesErrorPrefix(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "mygw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	gw.errorf("failed: %s", "reason")

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	want := "llmgw[mygw] ERROR failed: reason"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("want stderr to contain %q, got %q", want, buf.String())
	}
}
