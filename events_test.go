package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- eventRing: cap, order, copy semantics ---

// TestEventRing_AddSnapshot_OrderAndCap proves add overwrites the oldest
// entry once the ring exceeds eventRingCap, and snapshot returns the
// survivors newest-first.
func TestEventRing_AddSnapshot_OrderAndCap(t *testing.T) {
	r := &eventRing{}
	const total = eventRingCap + 10
	for i := 0; i < total; i++ {
		r.add(gatewayEvent{Message: fmt.Sprintf("event-%d", i)})
	}

	got := r.snapshot(eventRingCap)
	if len(got) != eventRingCap {
		t.Fatalf("len(got) = %d, want %d", len(got), eventRingCap)
	}
	if want := fmt.Sprintf("event-%d", total-1); got[0].Message != want {
		t.Errorf("got[0] = %q, want %q (newest first)", got[0].Message, want)
	}
	if want := fmt.Sprintf("event-%d", total-eventRingCap); got[len(got)-1].Message != want {
		t.Errorf("got[last] = %q, want %q (oldest surviving entry after the first 10 were overwritten)", got[len(got)-1].Message, want)
	}
}

// TestEventRing_Snapshot_LimitTruncates proves limit caps how many of the
// most recent entries snapshot returns, still newest-first.
func TestEventRing_Snapshot_LimitTruncates(t *testing.T) {
	r := &eventRing{}
	for i := 0; i < 10; i++ {
		r.add(gatewayEvent{Message: fmt.Sprintf("e%d", i)})
	}
	got := r.snapshot(3)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	want := []string{"e9", "e8", "e7"}
	for i, w := range want {
		if got[i].Message != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i].Message, w)
		}
	}
}

// TestEventRing_Snapshot_ReturnsFreshCopy proves a caller mutating its own
// snapshot slice never corrupts the ring's own backing array.
func TestEventRing_Snapshot_ReturnsFreshCopy(t *testing.T) {
	r := &eventRing{}
	r.add(gatewayEvent{Message: "orig"})

	got := r.snapshot(1)
	got[0].Message = "mutated"

	again := r.snapshot(1)
	if again[0].Message != "orig" {
		t.Errorf("snapshot()[0].Message = %q after an external mutation, want %q (snapshot must copy)", again[0].Message, "orig")
	}
}

// TestEventRing_Snapshot_Empty proves an empty ring answers a limit
// request with an empty, non-panicking slice.
func TestEventRing_Snapshot_Empty(t *testing.T) {
	r := &eventRing{}
	got := r.snapshot(50)
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// --- eventLog: push-rate throttle (Q3: 50/s/replica) ---

// TestEventLog_AllowPush_ThrottledAt50PerSecond proves eventPushPerSecond
// pushes are allowed within one second, the next is throttled, and a new
// second resets the window.
func TestEventLog_AllowPush_ThrottledAt50PerSecond(t *testing.T) {
	el := &eventLog{}
	now := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)

	for i := 0; i < eventPushPerSecond; i++ {
		if !el.allowPush(now) {
			t.Fatalf("push %d within the same second should be allowed", i)
		}
	}
	if el.allowPush(now) {
		t.Error("a push beyond eventPushPerSecond within the same second must be throttled")
	}
	if !el.allowPush(now.Add(time.Second)) {
		t.Error("a new second must reset the throttle window")
	}
}

// --- gatewayEvent: JSON round trip (Redis storage shape == API shape) ---

// TestGatewayEvent_JSONRoundTrip proves every field survives a
// marshal/unmarshal cycle unchanged — the exact contract eventLog.push/
// read depend on (the stored Redis payload IS the API view, with no
// translation step).
func TestGatewayEvent_JSONRoundTrip(t *testing.T) {
	ev := gatewayEvent{
		Time:     time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
		Replica:  "pod-1",
		Instance: "llmgw",
		User:     "alice",
		Group:    "eng",
		Model:    "openai/gpt-4o",
		Provider: "openai",
		Route:    routeChatCompletions,
		Kind:     eventKindUpstream,
		Message:  "upstream connection error",
		Status:   http.StatusBadGateway,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got gatewayEvent
	if unmarshalErr := json.Unmarshal(b, &got); unmarshalErr != nil {
		t.Fatalf("Unmarshal: %v", unmarshalErr)
	}
	gotB, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(gotB) != string(b) {
		t.Errorf("round trip mismatch:\n got %s\nwant %s", gotB, b)
	}
}

// TestGatewayEvent_JSON_OmitsOptionalFields proves a minimal event (no
// Instance/User/Group/Model/Provider/Status) omits those keys entirely,
// matching gatewayEvent's own omitempty tags — Route/Kind/Message/Time/
// Replica are the only fields a minimal event (e.g. the capacity hook)
// always carries.
func TestGatewayEvent_JSON_OmitsOptionalFields(t *testing.T) {
	ev := gatewayEvent{Route: routeCapacity, Kind: eventKindCapacity, Message: "server is at capacity; try again shortly"}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, absent := range []string{`"instance"`, `"user"`, `"group"`, `"model"`, `"provider"`, `"status"`} {
		if strings.Contains(string(b), absent) {
			t.Errorf("JSON = %s, must omit %s for an unset optional field", b, absent)
		}
	}
	for _, present := range []string{`"time"`, `"replica"`, `"route"`, `"kind"`, `"message"`} {
		if !strings.Contains(string(b), present) {
			t.Errorf("JSON = %s, must always include %s", b, present)
		}
	}
}

// --- recordUpstreamEvent: error classification (F3 hook 2) ---

// TestRecordUpstreamEvent_Classification drives every classification
// branch recordUpstreamEvent's own doc comment promises: a wrapped
// errProviderTimeout or context.DeadlineExceeded is a timeout/504; a
// *providerHTTPError is an upstream event only at 429 or >=500, never at
// an ordinary 4xx; *translateError and context.Canceled record nothing;
// anything else is upstream/502 with the message scrubbed via
// sanitizeProviderErr — proven here with a genuine credential-bearing
// *url.Error, matching the plan's own risk-section requirement (§6).
func TestRecordUpstreamEvent_Classification(t *testing.T) {
	// Built via net/url, not a literal credential-shaped string constant
	// (matches TestAdminOverview_BaseURLStripsCredentials' own reasoning,
	// admin_test.go): this is a fabricated test fixture, not a real
	// credential, but a literal userinfo URL string in source still
	// trips generic secret scanners.
	credentialBaseURL := (&url.URL{
		Scheme: "https",
		User:   url.UserPassword("secretuser", "secretpass"),
		Host:   "openai.invalid",
	}).String()

	// credentialAndKeyBaseURL additionally carries a "?key=" query
	// parameter — the timeout branch's own risk (verify-dash-backend.md
	// finding 2): before this fix only the generic (502) branch ran
	// lastErr through sanitizeProviderErr, so a timeout error embedding
	// this same base URL leaked BOTH the userinfo and any query-string
	// credential riding it (net/http embeds the dialed URL verbatim in a
	// *url.Error on a context-deadline failure). A separate provider
	// ("openai-keyed") carries this BaseURL so the existing "openai"
	// cases above are unaffected.
	credentialAndKeyBaseURL := (&url.URL{
		Scheme:   "https",
		User:     url.UserPassword("secretuser", "secretpass"),
		Host:     "openai.invalid",
		RawQuery: "key=leakyquerykey",
	}).String()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai":       {Type: "openai", BaseURL: credentialBaseURL, APIKey: "sk"},
		"openai-keyed": {Type: "openai", BaseURL: credentialAndKeyBaseURL, APIKey: "sk"},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.events.spawn = func(f func()) { f() } // deterministic: no Redis configured anyway, but keep this test independent of that fact

	watchdogTimeout := fmt.Errorf("%w: chat/completions: no upstream progress for 5m0s", errProviderTimeout)

	cases := []struct {
		err        error
		name       string
		provider   string // "" defaults to "openai" below
		wantMsgHas string
		wantKind   string
		wantStatus int
		wantEvent  bool
	}{
		{name: "wrapped errProviderTimeout -> timeout/504", err: watchdogTimeout, wantEvent: true, wantKind: eventKindTimeout, wantStatus: http.StatusGatewayTimeout},
		{name: "context.DeadlineExceeded -> timeout/504", err: fmt.Errorf("dial: %w", context.DeadlineExceeded), wantEvent: true, wantKind: eventKindTimeout, wantStatus: http.StatusGatewayTimeout},
		{name: "providerHTTPError 429 -> upstream/429", err: &providerHTTPError{status: http.StatusTooManyRequests}, wantEvent: true, wantKind: eventKindUpstream, wantStatus: http.StatusTooManyRequests},
		{name: "providerHTTPError 500 -> upstream/500", err: &providerHTTPError{status: http.StatusInternalServerError}, wantEvent: true, wantKind: eventKindUpstream, wantStatus: http.StatusInternalServerError},
		{name: "providerHTTPError 404 -> no event (ordinary client error)", err: &providerHTTPError{status: http.StatusNotFound}, wantEvent: false},
		{name: "providerHTTPError 400 -> no event (ordinary client error)", err: &providerHTTPError{status: http.StatusBadRequest}, wantEvent: false},
		{name: "translateError -> no event", err: &translateError{msg: "bad field"}, wantEvent: false},
		{name: "context.Canceled -> no event", err: context.Canceled, wantEvent: false},
		{name: "nil err -> no event", err: nil, wantEvent: false},
		{
			name:       "generic connection error -> upstream/502, credential scrubbed",
			err:        &url.Error{Op: "Get", URL: credentialBaseURL + "/v1/chat/completions", Err: errors.New("connection refused")},
			wantEvent:  true,
			wantKind:   eventKindUpstream,
			wantStatus: http.StatusBadGateway,
			wantMsgHas: "connection refused",
		},
		{
			// Finding 2 fix (verify-dash-backend.md): a *url.Error
			// wrapping context.DeadlineExceeded — net/http's own shape
			// for a context-deadline failure mid-dial — used to skip
			// sanitizeProviderErr entirely on the timeout branch, so
			// BOTH the userinfo (secretuser:secretpass@) and the
			// "?key=" query credential embedded in the dialed URL
			// leaked verbatim into the event feed.
			name:       "timeout: *url.Error wrapping DeadlineExceeded, userinfo and ?key= scrubbed",
			provider:   "openai-keyed",
			err:        &url.Error{Op: "Get", URL: credentialAndKeyBaseURL + "/v1/chat/completions", Err: context.DeadlineExceeded},
			wantEvent:  true,
			wantKind:   eventKindTimeout,
			wantStatus: http.StatusGatewayTimeout,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := c.provider
			if provider == "" {
				provider = "openai"
			}
			before := len(gw.events.ring.snapshot(eventRingCap))
			gw.recordUpstreamEvent(nil, "openai/gpt-4o", provider, routeChatCompletions, c.err)
			after := gw.events.ring.snapshot(eventRingCap)

			if !c.wantEvent {
				if len(after) != before {
					t.Fatalf("recorded an event for err=%v, want none: %+v", c.err, after[0])
				}
				return
			}
			if len(after) != before+1 {
				t.Fatalf("did not record an event for err=%v", c.err)
			}
			got := after[0] // newest first
			if got.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, c.wantKind)
			}
			if got.Status != c.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, c.wantStatus)
			}
			if c.wantMsgHas != "" && !strings.Contains(got.Message, c.wantMsgHas) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, c.wantMsgHas)
			}
			for _, leaked := range []string{"secretuser", "secretpass", "leakyquerykey"} {
				if strings.Contains(got.Message, leaked) {
					t.Errorf("Message leaks credential material %q: %q", leaked, got.Message)
				}
			}
		})
	}
}

// TestRecordProxyEvent_Classification covers recordProxyEvent's own
// status-based rule (F3 hook 3): 0/429/>=500 record an event, everything
// else (including a client-canceled result) does not.
func TestRecordProxyEvent_Classification(t *testing.T) {
	g := &Gateway{name: "llmgw", replica: "test-replica"}
	g.events = &eventLog{ring: &eventRing{}, nowFn: time.Now, spawn: func(f func()) { f() }}

	cases := []struct {
		wantKind   string
		result     proxyResult
		wantStatus int
		wantEvent  bool
	}{
		{result: proxyResult{status: 0}, wantEvent: true, wantKind: eventKindUpstream, wantStatus: http.StatusBadGateway},
		{result: proxyResult{status: http.StatusTooManyRequests}, wantEvent: true, wantKind: eventKindUpstream, wantStatus: http.StatusTooManyRequests},
		{result: proxyResult{status: http.StatusBadGateway}, wantEvent: true, wantKind: eventKindUpstream, wantStatus: http.StatusBadGateway},
		{result: proxyResult{status: http.StatusOK}, wantEvent: false},
		{result: proxyResult{status: http.StatusNotFound}, wantEvent: false},
		{result: proxyResult{status: 0, clientCanceled: true}, wantEvent: false},
		{result: proxyResult{status: http.StatusInternalServerError, clientCanceled: true}, wantEvent: false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("status=%d canceled=%v", c.result.status, c.result.clientCanceled), func(t *testing.T) {
			before := len(g.events.ring.snapshot(eventRingCap))
			g.recordProxyEvent(nil, "openai", routePassthrough, c.result)
			after := g.events.ring.snapshot(eventRingCap)
			if !c.wantEvent {
				if len(after) != before {
					t.Fatalf("recorded an event for %+v, want none", c.result)
				}
				return
			}
			if len(after) != before+1 {
				t.Fatalf("did not record an event for %+v", c.result)
			}
			got := after[0]
			if got.Kind != c.wantKind || got.Status != c.wantStatus {
				t.Errorf("got Kind=%q Status=%d, want Kind=%q Status=%d", got.Kind, got.Status, c.wantKind, c.wantStatus)
			}
		})
	}
}

// TestApplyScopeAttribution_FirstUserAndGroupWin proves
// applyScopeAttribution takes the FIRST user-kind and FIRST group-kind
// scope, ignoring a later total/other scope, matching
// buildLimitScopes/withTotalScope's own user-then-group-then-total order.
func TestApplyScopeAttribution_FirstUserAndGroupWin(t *testing.T) {
	scopes := []limitScope{
		{kind: "user", id: "alice"},
		{kind: "group", id: "eng"},
		{kind: totalScopeKind, id: totalScopeID},
	}
	var ev gatewayEvent
	applyScopeAttribution(&ev, scopes)
	if ev.User != "alice" || ev.Group != "eng" {
		t.Errorf("ev = %+v, want User=alice Group=eng", ev)
	}
}
