package traefikllmgateway

import (
	"errors"
	"sort"
	"testing"
)

func newRequirePrefixRegistry(t *testing.T, require bool) (*modelRegistry, map[string]providerAdapter) {
	t.Helper()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"beta":  newFakeAdapter("beta"),
	}
	cfg := &Config{
		RequireModelPrefix: require,
		ModelAliases:       map[string]string{"fast": "alpha/solo", "shared-alias": "shared"},
		Providers: map[string]*ProviderConfig{
			"alpha": {Models: []string{"shared", "solo"}},
			"beta":  {Models: []string{"shared", "other"}},
		},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	return reg, adapters
}

func TestModelRegistry_RequireModelPrefix_Resolve(t *testing.T) {
	t.Parallel()
	reg, adapters := newRequirePrefixRegistry(t, true)
	grp := allowAllGroup()

	for _, id := range []string{"solo", "shared", "other"} {
		if _, _, _, err := reg.resolve(id, grp); !errors.Is(err, errModelUnknown) {
			t.Errorf("resolve(%q) err = %v, want errModelUnknown", id, err)
		}
	}

	cases := []struct {
		id       string
		provider string
		canon    string
	}{
		{"alpha/solo", "alpha", "alpha/solo"},
		{"beta/shared", "beta", "beta/shared"},
		{"fast", "alpha", "alpha/solo"},
		{"shared-alias", "alpha", "alpha/shared"},
	}
	for _, tc := range cases {
		adapter, _, canon, err := reg.resolve(tc.id, grp)
		if err != nil {
			t.Errorf("resolve(%q): %v", tc.id, err)
			continue
		}
		if adapter != adapters[tc.provider] || canon != tc.canon {
			t.Errorf("resolve(%q) = %v %q, want %s %q", tc.id, adapter, canon, tc.provider, tc.canon)
		}
	}
}

func TestModelRegistry_RequireModelPrefix_ListFor_NoBareIDs(t *testing.T) {
	t.Parallel()
	reg, _ := newRequirePrefixRegistry(t, true)

	var got []string
	for _, entry := range reg.listFor(allowAllGroup()) {
		got = append(got, entry["id"].(string)) //nolint:forcetypeassert // modelObject always sets id to a string
	}
	sort.Strings(got)
	want := []string{"alpha/shared", "alpha/solo", "beta/other", "beta/shared", "fast", "shared-alias"}
	if len(got) != len(want) {
		t.Fatalf("listFor ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listFor ids = %v, want %v", got, want)
		}
	}
}

func TestModelRegistry_RequireModelPrefix_ListFor_RespectsProviderAllowList(t *testing.T) {
	t.Parallel()
	reg, _ := newRequirePrefixRegistry(t, true)
	grp := &group{name: "g", providers: []string{"beta"}}

	for _, entry := range reg.listFor(grp) {
		id := entry["id"].(string) //nolint:forcetypeassert // modelObject always sets id to a string
		if entry["owned_by"] != "beta" {
			t.Errorf("entry %q owned_by %v, want only beta-owned entries", id, entry["owned_by"])
		}
	}
	if _, _, _, err := reg.resolve("alpha/solo", grp); !errors.Is(err, errModelDenied) {
		t.Errorf("resolve(alpha/solo) err = %v, want errModelDenied", err)
	}
}

func TestModelRegistry_RequireModelPrefix_Off_KeepsBareIDs(t *testing.T) {
	t.Parallel()
	reg, _ := newRequirePrefixRegistry(t, false)

	if _, _, canon, err := reg.resolve("solo", allowAllGroup()); err != nil || canon != "alpha/solo" {
		t.Errorf("resolve(solo) = %q, %v, want alpha/solo, nil", canon, err)
	}
	var sawBare bool
	for _, entry := range reg.listFor(allowAllGroup()) {
		if entry["id"] == "solo" {
			sawBare = true
		}
	}
	if !sawBare {
		t.Error("listFor lost bare id \"solo\" with requireModelPrefix off")
	}
}
