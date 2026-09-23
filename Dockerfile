# Image for deployment form (B): the standalone compiled binary
# (cmd/gateway), which runs BEHIND Traefik rather than inside it. Form (A),
# the Yaegi-interpreted plugin, needs no image at all — Traefik loads this
# repository's source directly — so nothing here affects it.
#
# Build context is the repository root (both go.mod and vendor/ must be
# reachable); .dockerignore keeps webui/node_modules, .git and the
# integration fixtures out of the transfer.
#
# Built locally, never in CI: `make docker-build`. This repository
# deliberately does not add GitHub Actions for it, and .goreleaser.yaml
# stays at `builds: - skip: true` because its job is publishing the plugin
# SOURCE tarball for the Traefik catalog, not producing binaries.

# --platform=$BUILDPLATFORM pins the toolchain to the BUILDER's native
# architecture and cross-compiles from there via GOOS/GOARCH below. Without
# it, a multi-arch build runs the compiler itself under QEMU emulation for
# every foreign arch, which is dramatically slower for no benefit — Go
# cross-compiles natively.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

# Supplied by BuildKit for the arch currently being produced.
ARG TARGETOS
ARG TARGETARCH

# VERSION stamps main.version (cmd/gateway/main.go), the same var
# `make build-binary`'s own -ldflags -X main.version=$(BINARY_VERSION)
# stamps for the bin/llmgateway build (Makefile) — C3, review round 3,
# 2026-09. Left at its "dev" default when no --build-arg is passed
# (`make docker-build` passes BINARY_VERSION, below), so `llmgateway
# -version` inside the image matches whatever the caller stamped, instead
# of always printing "dev".
ARG VERSION=dev

WORKDIR /src

# -mod=vendor: vendor/ is committed (see .gitattributes' own comment for
# why it must stay that way), so the build never reaches the network and
# builds the exact dependency source this repository ships.
ENV CGO_ENABLED=0 GOFLAGS=-mod=vendor

COPY . .

# -trimpath strips local filesystem paths from the binary; -s -w drops the
# symbol and DWARF tables. CGO_ENABLED=0 above is what makes the result
# genuinely static and therefore runnable on distroless-static below.
# -X main.version=$VERSION matches build-binary's own stamping exactly
# (Makefile), so the two build paths can never disagree on what a
# "version" build-arg/var means.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
	go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/llmgateway ./cmd/gateway

# distroless-static carries CA certificates (needed: providers are reached
# over HTTPS) and nothing else — no shell, no package manager. :nonroot
# runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/llmgateway /usr/local/bin/llmgateway

USER nonroot:nonroot
EXPOSE 8080

# The config path, listen address and every other setting are overridable
# by flag or by the matching LLMGW_* environment variable (see
# cmd/gateway/main.go). Mount the config at this path, and mount the same
# secret volumes the plugin form uses — a `file:/llmgw/providers/...`
# apiKey resolves identically in both forms and needs the file to exist.
ENTRYPOINT ["/usr/local/bin/llmgateway"]
CMD ["-config", "/etc/llmgateway/config.yaml"]
