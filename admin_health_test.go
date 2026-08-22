package traefikllmgateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// findAdminProvider returns the entry named name from got.Providers, or
// fails the test — every admin_health_test.go case looks up exactly one
// provider by name out of newAdminTestConfig's fixed "zeta"/"alpha" pair.
func findAdminProvider(t *testing.T, got adminOverviewResponse, name string) *adminProviderView {
	t.Helper()
	for i := range got.Providers {
		if got.Providers[i].Name == name {
			return &got.Providers[i]
		}
	}
	t.Fatalf("providers missing %q: %+v", name, got.Providers)
	return nil
}

// TestAdminOverview_HealthState_DefaultClosed proves GET
// /admin/api/overview reports "closed" for every provider in a freshly
// constructed, never-failing Gateway (feat/provider-health) — the
// dashboard's default view for a healthy deployment is unaffected by this
// feature existing.
func TestAdminOverview_HealthState_DefaultClosed(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, name := range []string{"zeta", "alpha"} {
		p := findAdminProvider(t, got, name)
		if p.HealthState != "closed" {
			t.Errorf("provider %q HealthState = %q, want %q", name, p.HealthState, "closed")
		}
		if !p.OpenUntil.IsZero() {
			t.Errorf("provider %q OpenUntil = %v, want zero", name, p.OpenUntil)
		}
	}
}

// TestAdminOverview_HealthState_ReflectsOpenBreaker proves GET
// /admin/api/overview surfaces a tripped breaker (feat/provider-health) —
// the dashboard's whole reason for carrying these two fields — without
// affecting a healthy sibling provider's own view.
func TestAdminOverview_HealthState_ReflectsOpenBreaker(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Breaker = BreakerConfig{FailureThreshold: 2, OpenDuration: "1m", MaxOpenDuration: "10m"}
	h, gw := newAdminGatewayHandle(t, cfg)

	boom := errors.New("boom: discovery unreachable")
	st := gw.registry.states["alpha"]
	st.finishRefresh(gw.registry.now(), nil, boom)
	st.finishRefresh(gw.registry.now(), nil, boom) // reaches FailureThreshold: 2, opens the breaker

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	alpha := findAdminProvider(t, got, "alpha")
	if alpha.HealthState != "open" {
		t.Errorf("alpha HealthState = %q, want %q", alpha.HealthState, "open")
	}
	if alpha.OpenUntil.IsZero() {
		t.Error("alpha OpenUntil = zero, want a non-zero time while the breaker is open")
	}

	zeta := findAdminProvider(t, got, "zeta")
	if zeta.HealthState != "closed" {
		t.Errorf("zeta HealthState = %q, want %q — a sibling provider's breaker must not affect it", zeta.HealthState, "closed")
	}
}
