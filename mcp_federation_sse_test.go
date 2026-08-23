package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- unit: mcpAcceptsSSE / mcpNegotiateFormat ---

// TestMcpAcceptsSSE covers the tolerant-parsing requirements directly,
// without spinning up a full Gateway: q-values, surrounding whitespace,
// other media types sharing the list, case variation, a near-miss type
// that must NOT match by substring, multiple distinct "Accept:" header
// lines, and the no-header/empty case.
//
// MUTATION VERIFIED: temporarily changing the match to
// strings.Contains(strings.ToLower(header), "text/event-stream") (a
// naive whole-header substring check, the exact anti-pattern this
// function's own doc comment warns against) made the near-miss case
// below ("application/x-text/event-stream-foo" must NOT match) fail,
// since that string DOES contain "text/event-stream" as a substring.
// Reverted before committing.
func TestMcpAcceptsSSE(t *testing.T) {
	cases := []struct {
		name    string
		headers []string // one or more "Accept:" header LINES
		want    bool
	}{
		{name: "no Accept header at all", headers: nil, want: false},
		{name: "empty Accept header", headers: []string{""}, want: false},
		{name: "application/json alone", headers: []string{"application/json"}, want: false},
		{name: "exact text/event-stream alone", headers: []string{"text/event-stream"}, want: true},
		{
			name:    "production reproduction: application/json, text/event-stream",
			headers: []string{"application/json, text/event-stream"},
			want:    true,
		},
		{name: "different case", headers: []string{"TEXT/EVENT-STREAM"}, want: true},
		{name: "mixed case", headers: []string{"Text/Event-Stream"}, want: true},
		{name: "q-value parameter", headers: []string{"text/event-stream;q=0.9"}, want: true},
		{name: "q-value with space before it", headers: []string{"text/event-stream ;q=0.9"}, want: true},
		{
			name:    "q-values on both types, SSE not preferred but still acceptable",
			headers: []string{"application/json;q=0.1, text/event-stream;q=0.9"},
			want:    true,
		},
		{name: "extra whitespace around the comma-separated element", headers: []string{"application/json ,  text/event-stream  "}, want: true},
		{name: "wildcard alone does not count as an explicit SSE request", headers: []string{"*/*"}, want: false},
		{
			name:    "near-miss type must not substring-match",
			headers: []string{"application/x-text/event-stream-foo"},
			want:    false,
		},
		{
			name:    "near-miss type alongside a real other type",
			headers: []string{"application/json, application/x-text/event-stream-foo"},
			want:    false,
		},
		{
			name:    "two separate Accept header lines, second carries the real token",
			headers: []string{"application/json", "text/event-stream"},
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, federatedMCPPath, nil)
			for _, h := range tc.headers {
				req.Header.Add("Accept", h)
			}
			if got := mcpAcceptsSSE(req); got != tc.want {
				t.Errorf("mcpAcceptsSSE(Accept=%v) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
}

// TestMcpNegotiateFormat_StarStar_YieldsJSON pins the deliberate "*/*"
// decision at the negotiation layer (not just the lower-level parser
// above): a caller with no specific preference gets today's default,
// never a framing change it did not explicitly ask for.
//
// MUTATION VERIFIED: temporarily making mcpNegotiateFormat treat "*/*"
// as accepting SSE (return mcpResponseSSE whenever the header is
// non-empty) made this test fail, want mcpResponseJSON got
// mcpResponseSSE. Reverted before committing.
func TestMcpNegotiateFormat_StarStar_YieldsJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, federatedMCPPath, nil)
	req.Header.Set("Accept", "*/*")
	if got := mcpNegotiateFormat(req); got != mcpResponseJSON {
		t.Errorf("mcpNegotiateFormat(Accept: */*) = %v, want mcpResponseJSON — \"*/*\" states no specific preference and must not flip this route's default framing", got)
	}
}

// --- baseline: default framing is byte-identical to today's plain JSON ---
//
// These literal bytes were captured by running the CURRENT (pre-SSE-
// negotiation) handleMCPFederated against these exact requests before
// this file's own SSE-negotiation source changes existed, then pinned
// here verbatim ("assert against the actual current bytes", not merely
// against a re-parsed structural equivalent). Any change to the JSON
// path's byte output — reordering the envelope's fields, adding or
// dropping the trailing newline json.NewEncoder writes, a different
// Content-Type, or accidentally routing the JSON branch through the SSE
// writer — fails this test.
//
// MUTATION VERIFIED: temporarily forcing writeJSONRPCEnvelope's format
// check to `if true` (always taking the SSE branch, even for a plain
// mcpResponseJSON request) made every case below fail, since
// Content-Type became "text/event-stream" and the body gained a
// "data: " prefix and blank-line terminator. Reverted before committing.
func TestHandleMCPFederated_DefaultFraming_ByteIdenticalToBaseline(t *testing.T) {
	cases := []struct {
		name      string
		buildTest func(t *testing.T) (http.Handler, *http.Request)
		accept    string // "" means no Accept header at all
		wantBody  string
	}{
		{
			name:     "ping, no Accept header",
			wantBody: `{"jsonrpc":"2.0","result":{},"id":1}` + "\n",
			buildTest: func(t *testing.T) (http.Handler, *http.Request) {
				cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
				h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return h, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("1")})
			},
		},
		{
			name:     "malformed body parse error, Accept: application/json",
			accept:   "application/json",
			wantBody: `{"error":{"message":"parse error","code":-32700},"jsonrpc":"2.0","id":null}` + "\n",
			buildTest: func(t *testing.T) (http.Handler, *http.Request) {
				cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
				h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				req := httptest.NewRequest(http.MethodPost, federatedMCPPath, strings.NewReader("not json"))
				req.Header.Set("Authorization", "Bearer sk-alice")
				return h, req
			},
		},
		{
			name:     "initialize default version, no Accept header",
			wantBody: `{"jsonrpc":"2.0","result":{"capabilities":{"tools":{}},"protocolVersion":"2025-11-25","serverInfo":{"name":"traefik-llmgateway","version":"0.0.0-dev"}},"id":1}` + "\n",
			buildTest: func(t *testing.T) (http.Handler, *http.Request) {
				cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
				h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return h, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "initialize", ID: json.RawMessage("1")})
			},
		},
		{
			name:     "tools/list two-server aggregate, Accept: application/json",
			accept:   "application/json",
			wantBody: `{"jsonrpc":"2.0","result":{"tools":[{"name":"alpha_lookup","description":"alpha's tool"},{"name":"beta_search","description":"beta's tool"}]},"id":42}` + "\n",
			buildTest: func(t *testing.T) (http.Handler, *http.Request) {
				alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup", Description: "alpha's tool"}})
				beta := newMockJSONRPCServer(t, []mcpTool{{Name: "search", Description: "beta's tool"}})
				cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
				h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return h, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("42")})
			},
		},
		{
			name:     "tools/call unknown prefix error, no Accept header",
			wantBody: `{"error":{"message":"unknown tool: nosuch_tool","code":-32602},"jsonrpc":"2.0","id":9}` + "\n",
			buildTest: func(t *testing.T) (http.Handler, *http.Request) {
				cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
				h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				params, _ := json.Marshal(mcpToolCallParams{Name: "nosuch_tool"})
				return h, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("9"), Params: params})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, req := tc.buildTest(t)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want %q (default framing must not change)", ct, "application/json")
			}
			if rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want the exact pre-negotiation baseline byte sequence %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// --- SSE: explicit request gets SSE framing, with a correct terminator ---

// TestHandleMCPFederated_ProductionRepro_ToolsListAcceptEventStream is
// the literal reproduction from the incident report: a tools/list call
// carrying the client's own "Accept: application/json, text/event-stream"
// header must come back as SSE framing with a parseable "data:" line —
// not the plain JSON body that made the real client fail with "no data
// line in SSE response".
//
// MUTATION VERIFIED: reverting handleMCPFederated to call
// writeJSONRPCResult with a hardcoded mcpResponseJSON (ignoring the
// negotiated format entirely — the bug this whole change fixes) made
// this test fail: Content-Type stayed "application/json" and the body
// had no "data:" line. Reverted before committing.
func TestHandleMCPFederated_ProductionRepro_ToolsListAcceptEventStream(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup", Description: "alpha's tool"}})
	beta := newMockJSONRPCServer(t, []mcpTool{{Name: "search", Description: "beta's tool"}})
	cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("42")})
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}

	body := rec.Body.String()
	dataLine, ok := extractSingleSSEDataLine(t, body)
	if !ok {
		t.Fatalf("body has no parseable \"data:\" line: %q", body)
	}
	var got jsonrpcResponse
	if err := json.Unmarshal([]byte(dataLine), &got); err != nil {
		t.Fatalf("data line is not valid JSON-RPC: %v (line=%q)", err, dataLine)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Errorf("got %d tools, want 2 (alpha_lookup, beta_search)", len(result.Tools))
	}
}

// TestHandleMCPFederated_SSEFraming_TerminatesWithBlankLine isolates the
// exact defect class this whole change exists to fix — a missing SSE
// event terminator — as its own assertion, independent of payload
// content: the body must end with the blank-line terminator ("\n\n"
// after the data line), and must contain EXACTLY one such terminator
// (not, say, a terminator buried mid-body with trailing junk after it,
// which would equally break a client's per-event framing).
//
// MUTATION VERIFIED: temporarily changing sseWriter.writeData (sse.go) to
// append a single '\n' instead of two ("buf = append(buf, '\n')") made
// this test fail — body no longer ended with "\n\n" — reproducing
// exactly the missing-terminator bug class the brief warns against.
// Reverted before committing.
func TestHandleMCPFederated_SSEFraming_TerminatesWithBlankLine(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("1")})
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body does not end with the SSE blank-line terminator: %q", body)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("body does not start with the \"data: \" field prefix: %q", body)
	}
	// Exactly one event: the terminator ("\n\n") must appear only once,
	// at the very end — a stray earlier one would split this into two
	// (or more) malformed events instead of the single one intended.
	if n := strings.Count(body, "\n\n"); n != 1 {
		t.Errorf("body contains %d blank-line terminators, want exactly 1: %q", n, body)
	}
}

// TestHandleMCPFederated_ErrorResponses_HonourNegotiatedSSEFraming
// covers the brief's explicit requirement: "A client that asked for SSE
// must not get a JSON error it cannot parse." Both a locally-produced
// error (parse error, before any backend is ever contacted) and one
// produced deeper in tools/call's own resolution logic get the same SSE
// treatment as a success response.
//
// MUTATION VERIFIED: temporarily hardcoding format = mcpResponseJSON at
// the top of writeJSONRPCErrorResponse (ignoring its own parameter) made
// both cases below fail — Content-Type stayed "application/json".
// Reverted before committing.
func TestHandleMCPFederated_ErrorResponses_HonourNegotiatedSSEFraming(t *testing.T) {
	t.Run("parse error", func(t *testing.T) {
		cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
		h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, federatedMCPPath, strings.NewReader("not json"))
		req.Header.Set("Authorization", "Bearer sk-alice")
		req.Header.Set("Accept", "text/event-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want %q", ct, "text/event-stream")
		}
		dataLine, ok := extractSingleSSEDataLine(t, rec.Body.String())
		if !ok {
			t.Fatalf("body has no parseable \"data:\" line: %q", rec.Body.String())
		}
		var got jsonrpcResponse
		if err := json.Unmarshal([]byte(dataLine), &got); err != nil {
			t.Fatalf("data line is not valid JSON-RPC: %v", err)
		}
		if got.Error == nil || got.Error.Code != jsonrpcParseError {
			t.Errorf("error = %+v, want code %d", got.Error, jsonrpcParseError)
		}
	})

	t.Run("tools/call unknown prefix", func(t *testing.T) {
		cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
		h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		params, _ := json.Marshal(mcpToolCallParams{Name: "nosuch_tool"})
		req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("9"), Params: params})
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want %q", ct, "text/event-stream")
		}
		dataLine, ok := extractSingleSSEDataLine(t, rec.Body.String())
		if !ok {
			t.Fatalf("body has no parseable \"data:\" line: %q", rec.Body.String())
		}
		var got jsonrpcResponse
		if err := json.Unmarshal([]byte(dataLine), &got); err != nil {
			t.Fatalf("data line is not valid JSON-RPC: %v", err)
		}
		if got.Error == nil || got.Error.Code != jsonrpcInvalidParams {
			t.Errorf("error = %+v, want code %d", got.Error, jsonrpcInvalidParams)
		}
	})
}

// TestHandleMCPFederated_Notification_Returns202NoBody_EvenWithAcceptSSE
// pins the brief's other explicit warning: a notification's 202-with-
// no-body response must never get wrapped in an SSE frame, even when the
// caller's Accept header asks for one.
//
// MUTATION VERIFIED: temporarily replacing the notifications/* branch's
// `w.WriteHeader(http.StatusAccepted)` with
// `writeJSONRPCResult(w, format, nil, map[string]any{})` (wrapping it
// like every other response) made this test fail on both the status
// code changing to 200 and the body no longer being empty. Reverted
// before committing.
func TestHandleMCPFederated_Notification_Returns202NoBody_EvenWithAcceptSSE(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "notifications/initialized"})
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty — a notification must never be wrapped in an SSE frame", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Errorf("Content-Type = %q, must not be set to text/event-stream for a bodyless 202", ct)
	}
}

// extractSingleSSEDataLine finds the (single) "data:" line in an SSE
// body and returns its payload, reusing sse.go's own lastSSEDataLine so
// this test exercises the same parsing logic a well-behaved reader
// would, rather than a hand-rolled second parser that could disagree
// with it.
func extractSingleSSEDataLine(t *testing.T, body string) (string, bool) {
	t.Helper()
	data := lastSSEDataLine([]byte(body))
	if len(data) == 0 || string(data) == body {
		return "", false
	}
	return string(data), true
}
