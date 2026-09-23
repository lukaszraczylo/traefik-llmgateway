package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// adminCatalogPath and adminConsumersPath are GET /admin/api/catalog and
// GET /admin/api/consumers' routes (admin dashboard redesign, WP-B) —
// defined here, alongside the rest of these two features' own constants,
// mirroring adminEventsPath's own precedent (events.go).
const (
	adminCatalogPath   = "/admin/api/catalog"
	adminConsumersPath = "/admin/api/consumers"
)

// adminCatalogModel is one upstream model's read-only catalog row in GET
// /admin/api/catalog: ContextTokens is resolved display metadata
// (buildAdminModelMetaView, admin.go — the SAME resolution GET
// /admin/api/overview's own adminProviderView.ModelMeta already exposes,
// so the two views can never disagree about a model's context window),
// while InputPerMTokUSD/OutputPerMTokUSD/PriceSource are ALL the SAME
// BILLING resolution (billingPriceSource, pricing.go: modelMetaFree >
// override > builtinPricing > LiteLLM table > unpriced) — the exact
// price/source pair a real request against this model actually gets
// charged, never a separately-resolved display price.
//
// N1 fix (verify-redesign-final.md): an earlier version filled
// InputPerMTokUSD/OutputPerMTokUSD from buildAdminModelMetaView's own
// DISPLAY-layer resolution (operator override > discovery > built-in
// table) instead of billingPriceSource's, so a model with a `pricing:`
// override and no modelMeta/discovery price at all reported
// PriceSource: "override" alongside undefined prices — CostAvoidedCard's
// pickReferenceModel (lib/cost-avoided.ts) would select exactly that
// model as its default reference (its source is neither free nor
// unpriced) and then be unable to price anything against it, and
// PricingHealthTable showed "—" next to a badge claiming a real price
// source. Both fields are nil for `priceSourceFree`/`priceSourceUnpriced`
// (a genuinely free-or-unknown price has nothing to report), and the
// billingPriceSource-resolved price otherwise — always the SAME price
// billingPriceSource itself would report for a real request, by
// construction, never a second, independently-derived number.
type adminCatalogModel struct {
	ContextTokens    *int     `json:"contextTokens,omitempty"`
	InputPerMTokUSD  *float64 `json:"inputPerMTokUsd,omitempty"`
	OutputPerMTokUSD *float64 `json:"outputPerMTokUsd,omitempty"`
	ID               string   `json:"id"`
	Model            string   `json:"model"`
	PriceSource      string   `json:"priceSource"`
	Aliases          []string `json:"aliases,omitempty"`
	DisplayFree      bool     `json:"displayFree,omitempty"`
}

// adminCatalogProvider is one configured provider's catalog row in GET
// /admin/api/catalog — the SAME provider identity/health fields GET
// /admin/api/overview's adminProviderView already exposes (LastRefresh,
// OpenUntil, HealthState, DiscoveryEnabled), reused here rather than a
// second, divergent provider summary, plus its full model catalog
// (Models, above).
type adminCatalogProvider struct {
	LastRefresh      time.Time           `json:"lastRefresh"`
	OpenUntil        time.Time           `json:"openUntil"`
	Name             string              `json:"name"`
	Type             string              `json:"type"`
	HealthState      string              `json:"healthState"`
	Models           []adminCatalogModel `json:"models"`
	DiscoveryEnabled bool                `json:"discoveryEnabled"`
}

// adminCatalogResponse is the full body of GET /admin/api/catalog: every
// configured provider's model catalog, plus the SAME alias listing GET
// /admin/api/overview's own Aliases already carries
// (buildAdminAliasViews, admin.go) — the catalog is the Models/
// Reliability pages' own on-demand data source (plan §3.3: "catalog
// (/catalog on demand, 5-minute stale)"), distinct from the 5s-polled
// overview, which stays compact by design.
type adminCatalogResponse struct {
	Providers []adminCatalogProvider `json:"providers"`
	Aliases   []adminAliasView       `json:"aliases"`
}

// buildAdminCatalog assembles adminCatalogResponse from the SAME
// registry snapshot GET /admin/api/overview reads (g.registry.
// snapshot()) — no live counter read at all, unlike overview: every
// field here is either static configuration or the registry's own
// discovery state, so this endpoint needs no limiter round trip.
func (g *Gateway) buildAdminCatalog() adminCatalogResponse {
	snaps := g.registry.snapshot()

	// aliasesByTarget groups every configured alias by its configured
	// Target string (ModelAliases' own doc comment, llmgateway.go: a
	// target names a canonical "provider/model" id), so each model's own
	// Aliases field below is a single map lookup rather than a full scan
	// of the alias list per model.
	aliasesByTarget := make(map[string][]string)
	for _, a := range g.registry.aliasSnapshot() {
		aliasesByTarget[a.Target] = append(aliasesByTarget[a.Target], a.Alias)
	}

	providers := make([]adminCatalogProvider, len(snaps))
	for i, s := range snaps {
		models := make([]adminCatalogModel, len(s.models))
		for j, model := range s.models {
			canonical := providerModelScopeID(s.name, model)
			metaView := buildAdminModelMetaView(g.registry.resolveMetaFor(s.name, model))
			source, price := billingPriceSource(canonical, model, g.cfg.Pricing, g.cfg.ModelMeta)
			displayFree := source == priceSourceUnpriced && strings.HasSuffix(model, freeTierSuffix)

			// N1 fix: InputPerMTokUSD/OutputPerMTokUSD report the SAME
			// price billingPriceSource just resolved, never
			// metaView's own display-layer price — see
			// adminCatalogModel's own doc comment. Left nil for
			// priceSourceFree/priceSourceUnpriced: there is no real
			// per-token rate to report for either (billingPriceSource
			// itself returns the zero ModelPricing for both), and a
			// nil price here is exactly what every UI consumer
			// (priceCell in both ModelCatalogTable and
			// PricingHealthTable) already renders as "—" instead of a
			// misleading "$0.00".
			var inputPerM, outputPerM *float64
			if source != priceSourceFree && source != priceSourceUnpriced {
				in := price.InputPerM
				out := price.OutputPerM
				inputPerM = &in
				outputPerM = &out
			}

			var aliases []string
			if names := aliasesByTarget[canonical]; len(names) > 0 {
				aliases = append(aliases, names...)
				sort.Strings(aliases)
			}

			models[j] = adminCatalogModel{
				ID:               canonical,
				Model:            model,
				ContextTokens:    metaView.ContextTokens,
				InputPerMTokUSD:  inputPerM,
				OutputPerMTokUSD: outputPerM,
				PriceSource:      source,
				DisplayFree:      displayFree,
				Aliases:          aliases,
			}
		}
		providers[i] = adminCatalogProvider{
			Name:             s.name,
			Type:             s.typeName,
			LastRefresh:      s.lastRefresh,
			OpenUntil:        s.openUntil,
			HealthState:      s.health.String(),
			DiscoveryEnabled: s.discoveryEnabled,
			Models:           models,
		}
	}

	return adminCatalogResponse{Providers: providers, Aliases: g.buildAdminAliasViews()}
}

// serveAdminCatalog writes buildAdminCatalog's result as JSON.
func (g *Gateway) serveAdminCatalog(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminCatalog())
}

// adminConsumerUser is one active API-key holder's read-only row in GET
// /admin/api/consumers — Source/Admin/Providers/Models sourced from
// userSummary's own new fields (auth.go's doc comment has the full
// source/personal-grant contract).
type adminConsumerUser struct {
	Limits    *LimitsConfig `json:"limits,omitempty"`
	Name      string        `json:"name"`
	Source    string        `json:"source"` // inline|file
	Groups    []string      `json:"groups"`
	Providers []string      `json:"providers,omitempty"`
	Models    []string      `json:"models,omitempty"`
	Admin     bool          `json:"admin,omitempty"`
}

// adminModelAccessCount is one group's per-provider model-access tally in
// GET /admin/api/consumers: Allowed (how many of that provider's
// catalogued models group.allowsProviderModel authorizes) out of Total
// (that provider's full catalogued model count) — so the Access Matrix
// page can render "12/40 models" beside a provider a group is only
// partially restricted to, without the response carrying the full
// per-model boolean grid.
type adminModelAccessCount struct {
	Allowed int `json:"allowed"`
	Total   int `json:"total"`
}

// adminConsumerGroup is one configured group's read-only row in GET
// /admin/api/consumers: its configured access lists (groupSummary's own
// doc comment, auth.go) plus two RESOLVED views neither GET
// /admin/api/overview nor GET /admin/api/usage already expose —
// AllowedProviders (every currently catalogued provider name this group
// can reach at all, never nil) and ModelAccess (per-provider allowed/
// total model tallies, adminModelAccessCount above) — both computed
// against the LIVE registry catalog, not merely echoing the group's own
// configured glob list the way Providers/Models below do.
type adminConsumerGroup struct {
	Limits           *LimitsConfig                    `json:"limits,omitempty"`
	ModelAccess      map[string]adminModelAccessCount `json:"modelAccess"`
	Name             string                           `json:"name"`
	Providers        []string                         `json:"providers,omitempty"`
	Models           []string                         `json:"models,omitempty"`
	MCPServers       []string                         `json:"mcpServers,omitempty"`
	Agents           []string                         `json:"agents,omitempty"`
	PassthroughPaths []string                         `json:"passthroughPaths,omitempty"`
	// AllowedProviders is never nil — an empty (but non-nil) slice for a
	// group the live catalog currently has no configured provider for,
	// distinct from a JSON "null" a stale client build might otherwise
	// mistake for "not yet loaded" (adminTargetView.Access's own doc
	// comment, admin.go, documents the identical null-vs-empty
	// distinction for a different field).
	AllowedProviders []string `json:"allowedProviders"`
	MemberCount      int      `json:"memberCount"`
}

// adminConsumersResponse is the full body of GET /admin/api/consumers:
// every currently active user and every configured group, redaction-safe
// (auth.go's userSummary/groupSummary doc comments), plus the configured
// users-file PATH ONLY — never its contents (plan §6's own "usersFile
// path shown, contents never" risk note).
type adminConsumersResponse struct {
	UsersFile string               `json:"usersFile,omitempty"`
	Users     []adminConsumerUser  `json:"users"`
	Groups    []adminConsumerGroup `json:"groups"`
}

// buildAdminConsumers assembles adminConsumersResponse: authStore.
// snapshot's own listing (userSummary/groupSummary, auth.go) for the
// redaction-safe fields, plus the live registry catalog (g.registry.
// snapshot()) for AllowedProviders/ModelAccess — computed by calling the
// group's own allowsProvider/allowsProviderModel methods DIRECTLY
// against authStore's internal groups map (g.auth.groups, same package):
// those methods are the exact request-path authorization check
// (allowsProviderModel's own doc comment, auth.go), so this view can
// never authorize (or restrict) a provider/model pair differently than a
// real request would.
func (g *Gateway) buildAdminConsumers() adminConsumersResponse {
	userSummaries, groupSummaries := g.auth.snapshot()
	snaps := g.registry.snapshot()

	users := make([]adminConsumerUser, len(userSummaries))
	for i, us := range userSummaries {
		users[i] = adminConsumerUser{
			Limits:    us.limits,
			Name:      us.name,
			Source:    us.source,
			Groups:    us.groups,
			Providers: us.personalProviders,
			Models:    us.personalModels,
			Admin:     us.admin,
		}
	}

	groups := make([]adminConsumerGroup, len(groupSummaries))
	for i, gs := range groupSummaries {
		grp := g.auth.groups[gs.name]
		allowedProviders := make([]string, 0, len(snaps))
		modelAccess := make(map[string]adminModelAccessCount, len(snaps))
		for _, s := range snaps {
			allowed := 0
			for _, model := range s.models {
				if grp != nil && grp.allowsProviderModel(s.name, model) {
					allowed++
				}
			}
			modelAccess[s.name] = adminModelAccessCount{Allowed: allowed, Total: len(s.models)}
			if grp != nil && grp.allowsProvider(s.name) {
				allowedProviders = append(allowedProviders, s.name)
			}
		}
		sort.Strings(allowedProviders)
		groups[i] = adminConsumerGroup{
			Limits:           gs.limits,
			Name:             gs.name,
			Providers:        gs.providers,
			Models:           gs.models,
			MCPServers:       gs.mcpServers,
			Agents:           gs.agents,
			PassthroughPaths: gs.passthroughPaths,
			AllowedProviders: allowedProviders,
			ModelAccess:      modelAccess,
			MemberCount:      gs.memberCount,
		}
	}

	var usersFile string
	if g.cfg.Users != nil {
		usersFile = g.cfg.Users.File
	}

	return adminConsumersResponse{Users: users, Groups: groups, UsersFile: usersFile}
}

// serveAdminConsumers writes buildAdminConsumers's result as JSON.
func (g *Gateway) serveAdminConsumers(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminConsumers())
}
