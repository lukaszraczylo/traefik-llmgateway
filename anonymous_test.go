package traefikllmgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func anonAuthCfg() *Config {
	return &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups: map[string]*GroupConfig{
			"eng":    {},
			"public": {Models: []string{"gpt-5-mini"}, MemberLimits: &LimitsConfig{RequestsPerMinute: 7}},
		},
		Users: &UsersConfig{
			AnonymousGroup: "public",
			Inline:         []*UserConfig{{Name: "a", Group: "eng", APIKey: "sk-secret"}},
		},
	}
}

func TestIdentify_AnonymousMode(t *testing.T) {
	a, err := newAuthStore(anonAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		headers   map[string]string
		name      string
		wantUser  string
		wantGroup string
		wantOK    bool
	}{
		{name: "no credentials", wantOK: true, wantUser: anonymousUserName, wantGroup: "public"},
		{name: "valid bearer", headers: map[string]string{"Authorization": "Bearer sk-secret"}, wantOK: true, wantUser: "a", wantGroup: "eng"},
		{name: "valid x-api-key", headers: map[string]string{"x-api-key": "sk-secret"}, wantOK: true, wantUser: "a", wantGroup: "eng"},
		{name: "wrong bearer", headers: map[string]string{"Authorization": "Bearer sk-wrong"}},
		{name: "wrong x-api-key", headers: map[string]string{"x-api-key": "sk-wrong"}},
		{name: "non-bearer scheme", headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}},
		{name: "empty bearer", headers: map[string]string{"Authorization": "Bearer "}},
		{name: "blank x-api-key", headers: map[string]string{"x-api-key": "   "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			u, grp, ok := a.identify(r)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if u.name != tc.wantUser || grp.name != tc.wantGroup {
				t.Fatalf("got user %q group %q, want %q %q", u.name, grp.name, tc.wantUser, tc.wantGroup)
			}
		})
	}
}

func TestIdentify_AnonymousPrincipalCarriesGroupGrantAndLimits(t *testing.T) {
	a, err := newAuthStore(anonAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	u, grp, ok := a.identify(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !ok {
		t.Fatal("want anonymous principal")
	}
	if u.admin {
		t.Fatal("anonymous principal must never be admin")
	}
	if u.limits == nil || u.limits.RequestsPerMinute != 7 {
		t.Fatalf("limits = %+v, want the group's memberLimits (RequestsPerMinute 7)", u.limits)
	}
	if !grp.allowsProviderModel("openai", "gpt-5-mini") || grp.allowsProviderModel("openai", "gpt-5") {
		t.Fatal("anonymous principal must carry exactly the public group's grant")
	}
}

func TestIdentifyKeyed_NeverAnonymous(t *testing.T) {
	a, err := newAuthStore(anonAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.identifyKeyed(httptest.NewRequest(http.MethodGet, adminOverviewPath, nil)); ok {
		t.Fatal("identifyKeyed must not admit a keyless request")
	}
	r := httptest.NewRequest(http.MethodGet, adminOverviewPath, nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	if u, _, ok := a.identifyKeyed(r); !ok || u.name != "a" {
		t.Fatalf("identifyKeyed with a valid key: user %v ok %v", u, ok)
	}
}

func TestNewAuthStore_AnonymousGroupValidation(t *testing.T) {
	t.Run("unknown group", func(t *testing.T) {
		cfg := anonAuthCfg()
		cfg.Users.AnonymousGroup = "nope"
		_, err := newAuthStore(cfg)
		if err == nil || !strings.Contains(err.Error(), `anonymousGroup references unknown group "nope"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("inline user named anonymous", func(t *testing.T) {
		cfg := anonAuthCfg()
		cfg.Users.Inline = append(cfg.Users.Inline, &UserConfig{Name: anonymousUserName, Group: "eng", APIKey: "sk-other"})
		_, err := newAuthStore(cfg)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("name allowed when mode off", func(t *testing.T) {
		cfg := anonAuthCfg()
		cfg.Users.AnonymousGroup = ""
		cfg.Users.Inline = append(cfg.Users.Inline, &UserConfig{Name: anonymousUserName, Group: "eng", APIKey: "sk-other"})
		if _, err := newAuthStore(cfg); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("file user named anonymous", func(t *testing.T) {
		a, err := newAuthStore(anonAuthCfg())
		if err != nil {
			t.Fatal(err)
		}
		err = a.replaceFileUsers([]*UserConfig{{Name: anonymousUserName, Group: "eng", APIKey: "sk-file"}})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGateway_AnonymousMode(t *testing.T) {
	cfg := newAdminTestConfig()
	cfg.Groups["public"] = &GroupConfig{Models: []string{"a-model-1"}}
	cfg.Users.AnonymousGroup = "public"
	cfg.Metrics = &MetricsConfig{Enabled: true}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/v1/models", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models without a key: status %d, body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "a-model-1") || strings.Contains(body, "z-model") {
		t.Fatalf("/v1/models must list only the anonymous group's models: %s", body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/v1/models", "sk-wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models with a wrong key: status %d, want 401", rec.Code)
	}

	for _, p := range []string{adminOverviewPath, adminUsagePath, adminTargetsPath, metricsPath(cfg)} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, p, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a key: status %d, want 401", p, rec.Code)
		}
	}
}

func TestGateway_AnonymousModeOffKeepsKeyRequired(t *testing.T) {
	h, _ := newAdminGatewayHandle(t, newAdminTestConfig())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/v1/models", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}
