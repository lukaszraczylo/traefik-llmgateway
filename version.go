package traefikllmgateway

// devPluginVersion is the placeholder carried by source-tree, local and
// test builds. Anonymous usage telemetry is suppressed while pluginVersion
// still reports this sentinel, so only stamped release builds emit a
// "plugin loaded" ping — a developer running the test suite, or an
// operator building from a checkout, never phones home.
const devPluginVersion = "0.0.0-dev"

// pluginVersion is the released version of this plugin. It is stamped at
// release time by ./workflow-prepare.sh (invoked by the shared go-release
// workflow before GoReleaser builds and tags), which rewrites the string
// below to the computed semver.
//
// Traefik runs this plugin under Yaegi, where the version cannot be
// resolved from build info at runtime (debug.ReadBuildInfo sees Traefik's
// build graph, not the interpreted plugin). This build-stamped constant is
// therefore the single source of truth for the version surfaced read-only
// in GET /admin/api/overview (spec §4, v0.2) and reported by anonymous
// usage telemetry (llmgateway.go). Local/dev/test builds carry the
// devPluginVersion sentinel above, since nothing has stamped them.
//
// The literal below must stay byte-identical to devPluginVersion:
// workflow-prepare.sh rewrites THIS line by name, and telemetry compares
// the two constants to decide whether the build was stamped.
const pluginVersion = "1.0.4"
