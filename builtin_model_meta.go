package traefikllmgateway

// builtinModelMeta is one built-in, LiteLLM-synced catalog entry (feature
// v0.23): a model's context window and per-token cost, in the same
// micro-USD-per-million-tokens unit ModelMetaConfig uses (modelmeta.go's
// microUSDPerUSD). Zero in any field means "this table has no data for
// it" — resolveModelMeta (modelmeta.go) only ever trusts a field greater
// than zero, never a bare zero value, so there is no ambiguity with a
// genuinely free or zero-context entry (this table never has one:
// pricing_data_gen.go's generator only ever emits a field it found a
// positive upstream value for). Regenerated wholesale by `make
// pricing-sync` (tools/pricing-sync) — see pricing_data_gen.go's own
// header for the source snapshot and pruning rule, and
// tools/pricing-sync/main.go's own doc comment for the full pruning/
// naming-bridge design.
//
// Kept in its own file, separate from modelmeta.go's resolution logic:
// this type is the generated table's own data shape, needed to compile
// pricing_data_gen.go on its own, independent of the resolveModelMeta/
// registry wiring layered on top of it.
type builtinModelMeta struct {
	ContextTokens             int
	InputCostPerMTokMicroUSD  int64
	OutputCostPerMTokMicroUSD int64
}
