package traefikllmgateway

// pluginVersion is the released version of this plugin. It is stamped at
// release time by ./workflow-prepare.sh (invoked by the shared go-release
// workflow before GoReleaser builds and tags), which rewrites the string
// below to the computed semver.
//
// Traefik runs this plugin under Yaegi, where the version cannot be
// resolved from build info at runtime (debug.ReadBuildInfo sees Traefik's
// build graph, not the interpreted plugin). This build-stamped constant is
// therefore the single source of truth for the version surfaced read-only
// in GET /admin/api/overview (spec §4, v0.2). Local/dev/test builds carry
// the "0.0.0-dev" sentinel below, since nothing has stamped them. Not read
// from anywhere else in the package.
const pluginVersion = "1.0.2"
