package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// --- GET /admin/api/catalog ---

func TestAdminCatalog_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"non-admin", "sk-alice", http.StatusForbidden},
		{"admin", "sk-admin1", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminCatalogPath, c.apiKey))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestAdminCatalog_PriceSourceAndDisplayFree exercises the five billing
// price sources plus the ':free'-suffix display-only case (DECISIONS Q3)
// end to end through GET /admin/api/catalog — including N1's own
// regression (verify-redesign-final.md): "alpha/a-model-1" carries a
// `pricing:` override and NO modelMeta/discovery entry at all, so an
// earlier version (InputPerMTokUSD/OutputPerMTokUSD filled from
// buildAdminModelMetaView's own display-layer resolution instead of
// billingPriceSource's) reported PriceSource: "override" alongside nil
// prices — this table now pins both price fields to the ACTUAL billed
// rate for every priced source, and nil for free/unpriced.
func TestAdminCatalog_PriceSourceAndDisplayFree(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Pricing = map[string]*ModelPricing{
		"alpha/a-model-1": {InputPerM: 5, OutputPerM: 5}, // override on canonical, no modelMeta/discovery price at all
	}
	cfg.ModelMeta = map[string]*ModelMetaConfig{
		"alpha/a-model-2": {Free: true}, // modelMeta free
	}
	cfg.Providers["zeta"].Models = []string{"z-model", "gpt-5", "some-model:free"}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminCatalogPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := map[string]adminCatalogModel{}
	for _, p := range got.Providers {
		for _, m := range p.Models {
			byID[m.ID] = m
		}
	}

	builtinGPT5 := builtinPricing["gpt-5"]
	cases := []struct {
		wantInputPerM   *float64 // nil means "must be omitted/nil"
		wantOutputPerM  *float64
		id              string
		wantSource      string
		wantDisplayFree bool
	}{
		{ptrFloat64(5), ptrFloat64(5), "alpha/a-model-1", priceSourceOverride, false},
		{nil, nil, "alpha/a-model-2", priceSourceFree, false},
		{ptrFloat64(builtinGPT5.InputPerM), ptrFloat64(builtinGPT5.OutputPerM), "zeta/gpt-5", priceSourceBuiltin, false},
		{nil, nil, "zeta/some-model:free", priceSourceUnpriced, true},
		{nil, nil, "zeta/z-model", priceSourceUnpriced, false},
	}
	for _, c := range cases {
		got, ok := byID[c.id]
		if !ok {
			t.Errorf("model %q missing from catalog", c.id)
			continue
		}
		if got.PriceSource != c.wantSource {
			t.Errorf("model %q PriceSource = %q, want %q", c.id, got.PriceSource, c.wantSource)
		}
		if got.DisplayFree != c.wantDisplayFree {
			t.Errorf("model %q DisplayFree = %v, want %v", c.id, got.DisplayFree, c.wantDisplayFree)
		}
		if !ptrFloat64Equal(got.InputPerMTokUSD, c.wantInputPerM) {
			t.Errorf("model %q InputPerMTokUSD = %v, want %v", c.id, derefFloat64(got.InputPerMTokUSD), derefFloat64(c.wantInputPerM))
		}
		if !ptrFloat64Equal(got.OutputPerMTokUSD, c.wantOutputPerM) {
			t.Errorf("model %q OutputPerMTokUSD = %v, want %v", c.id, derefFloat64(got.OutputPerMTokUSD), derefFloat64(c.wantOutputPerM))
		}
	}
}

// ptrFloat64/derefFloat64/ptrFloat64Equal are small nil-safe *float64
// helpers for TestAdminCatalog_PriceSourceAndDisplayFree's own table —
// adminCatalogModel's price fields are `*float64` (nil = "no billed
// rate", never a fabricated 0), so the table needs to express and
// compare that nil-ness directly rather than comparing bare float64s.
func ptrFloat64(v float64) *float64 { return &v }

func derefFloat64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func ptrFloat64Equal(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestAdminCatalog_AliasesGroupedByTarget pins each model's Aliases field
// to exactly the configured aliases whose Target names it, sorted.
func TestAdminCatalog_AliasesGroupedByTarget(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.ModelAliases = map[string]string{
		"z-model-b-alias": "zeta/z-model",
		"z-model-a-alias": "zeta/z-model",
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminCatalogPath, "sk-admin1"))
	var got adminCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var zModel *adminCatalogModel
	for _, p := range got.Providers {
		for i := range p.Models {
			if p.Models[i].ID == "zeta/z-model" {
				zModel = &p.Models[i]
			}
		}
	}
	if zModel == nil {
		t.Fatal("zeta/z-model missing from catalog")
	}
	want := []string{"z-model-a-alias", "z-model-b-alias"}
	if len(zModel.Aliases) != len(want) || zModel.Aliases[0] != want[0] || zModel.Aliases[1] != want[1] {
		t.Errorf("Aliases = %v, want %v (sorted)", zModel.Aliases, want)
	}
	if len(got.Aliases) != 2 {
		t.Errorf("top-level Aliases = %d entries, want 2", len(got.Aliases))
	}
}

// --- GET /admin/api/consumers ---

func TestAdminConsumers_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"non-admin", "sk-alice", http.StatusForbidden},
		{"admin", "sk-admin1", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConsumersPath, c.apiKey))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestAdminConsumers_UserSourceAndPersonalGrant exercises source
// (inline/file) and the personal-grant Providers/Models fields end to
// end through GET /admin/api/consumers.
func TestAdminConsumers_UserSourceAndPersonalGrant(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Users.Inline = append(cfg.Users.Inline, &UserConfig{
		Name: "grantee", Group: "agroup", APIKey: "sk-grantee",
		Providers: []string{"alpha"}, Models: []string{"a-model-1"},
	})
	h, gw := newAdminGatewayHandle(t, cfg)
	if err := gw.auth.replaceFileUsers([]*UserConfig{
		{Name: "filed", Group: "agroup", APIKey: "sk-filed"},
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConsumersPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminConsumersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]adminConsumerUser{}
	for _, u := range got.Users {
		byName[u.Name] = u
	}

	if byName["alice"].Source != "inline" {
		t.Errorf(`alice.Source = %q, want "inline"`, byName["alice"].Source)
	}
	if byName["filed"].Source != "file" {
		t.Errorf(`filed.Source = %q, want "file"`, byName["filed"].Source)
	}
	if !byName["admin1"].Admin {
		t.Errorf("admin1.Admin = false, want true")
	}
	g := byName["grantee"]
	if len(g.Providers) != 1 || g.Providers[0] != "alpha" {
		t.Errorf("grantee.Providers = %v, want [alpha]", g.Providers)
	}
	if len(g.Models) != 1 || g.Models[0] != "a-model-1" {
		t.Errorf("grantee.Models = %v, want [a-model-1]", g.Models)
	}
}

// TestAdminConsumers_ModelAccessCounts pins adminConsumerGroup's
// ModelAccess/AllowedProviders against the live catalog for a group with
// an explicit provider+model restriction.
func TestAdminConsumers_ModelAccessCounts(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Groups["agroup"] = &GroupConfig{Providers: []string{"alpha"}, Models: []string{"a-model-1"}}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConsumersPath, "sk-admin1"))
	var got adminConsumersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var agroup *adminConsumerGroup
	for i := range got.Groups {
		if got.Groups[i].Name == "agroup" {
			agroup = &got.Groups[i]
		}
	}
	if agroup == nil {
		t.Fatal("agroup missing from consumers response")
	}
	if agroup.AllowedProviders == nil {
		t.Error("AllowedProviders = nil, want non-nil")
	}
	if len(agroup.AllowedProviders) != 1 || agroup.AllowedProviders[0] != "alpha" {
		t.Errorf("AllowedProviders = %v, want [alpha]", agroup.AllowedProviders)
	}
	alpha, ok := agroup.ModelAccess["alpha"]
	if !ok {
		t.Fatal("ModelAccess[alpha] missing")
	}
	if alpha.Allowed != 1 || alpha.Total != 2 {
		t.Errorf("ModelAccess[alpha] = %+v, want {Allowed:1 Total:2}", alpha)
	}
	zeta, ok := agroup.ModelAccess["zeta"]
	if !ok {
		t.Fatal("ModelAccess[zeta] missing")
	}
	if zeta.Allowed != 0 || zeta.Total != 1 {
		t.Errorf("ModelAccess[zeta] = %+v, want {Allowed:0 Total:1}", zeta)
	}

	// zgroup has no restriction: unrestricted access to every provider.
	var zgroup *adminConsumerGroup
	for i := range got.Groups {
		if got.Groups[i].Name == "zgroup" {
			zgroup = &got.Groups[i]
		}
	}
	if zgroup == nil {
		t.Fatal("zgroup missing")
	}
	if len(zgroup.AllowedProviders) != 2 {
		t.Errorf("zgroup.AllowedProviders = %v, want both providers", zgroup.AllowedProviders)
	}
}

// TestAdminConsumers_UsersFilePathOnly pins the "usersFile path shown,
// contents never" risk note (plan §6): only the configured path appears,
// never any per-user data beyond what userSummary/groupSummary already
// carry.
func TestAdminConsumers_UsersFilePathOnly(t *testing.T) {
	t.Parallel()
	fp := filepath.Join(t.TempDir(), "users.json")
	writeUsersDoc(t, fp, []*UserConfig{{Name: "filed", Group: "agroup", APIKey: "sk-filed"}}, time.Time{})

	cfg := newAdminTestConfig()
	cfg.Users.File = fp
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConsumersPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminConsumersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UsersFile != fp {
		t.Errorf("UsersFile = %q, want %q", got.UsersFile, fp)
	}
	for _, u := range got.Users {
		if u.Name == "filed" && u.Source != "file" {
			t.Errorf("filed.Source = %q, want file", u.Source)
		}
	}
}
