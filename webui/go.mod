// This go.mod exists only to wall webui/ off as a separate Go module
// boundary: without it, the repo root's `go build ./...`/`go vet ./...`/
// `go test ./...` (and any tool honoring the same `./...` pattern, e.g.
// golangci-lint) would descend into webui/node_modules — an npm
// dependency tree that happens to bundle a stray .go file
// (node_modules/flatted/golang/pkg/flatted/flatted.go) — and choke on
// code this repo neither wrote nor can build. Go's module resolution
// stops `./...` at a nested go.mod by design, so this one file is the
// entire fix; nothing here is ever built or imported by anything.
module github.com/lukaszraczylo/traefik-llmgateway/webui-nodemodules-boundary

go 1.22
