# Serialize targets even under `make -j`: the integration targets are
# stateful (one docker compose stack, one set of host ports) and running
# two of them concurrently would corrupt each other's containers.
.NOTPARALLEL:

.PHONY: test lint yaegi-check admin-ui pricing-sync integration integration-keep integration-up integration-wait integration-down build-binary docker-build run-local

test:
	go test ./... -count=1

lint:
	@fmt_out="$$(gofmt -l .)"; \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt: files not formatted:"; \
		echo "$$fmt_out"; \
		exit 1; \
	fi
	go vet ./...

# admin-ui rebuilds the Vue admin panel (webui/) and regenerates
# admin_assets_gen.go — the repo-root Go file admin.go's serveAdminPage/
# serveAdminAsset actually serve. Only ever needed after a webui/ source
# change; CI never runs this (admin_assets_gen.go is committed, and
# .gitattributes export-ignores webui/ from release source tarballs — see
# its own comment), so CI never needs node. `npm ci` requires
# webui/package-lock.json, which is committed for exactly this.
admin-ui:
	cd webui && npm ci && npm run build
	node webui/generate.mjs
	gofmt -w admin_assets_gen.go

yaegi-check:
	cd tools/yaegi-check && GOWORK=off go run . $(CURDIR)

# pricing-sync regenerates the repo-root pricing_data_gen.go: the
# built-in, LiteLLM-synced per-model context/cost table feature v0.23's
# metadata resolution falls back to (modelmeta.go's resolveModelMeta).
# Fetches LiteLLM's model_prices_and_context_window.json at BUILD TIME
# only — see tools/pricing-sync/main.go's own doc comment for the
# pruning rule and naming-bridge mechanism. Not run by CI (network
# fetch, and the generated table is committed); an operator re-runs this
# by hand when LiteLLM's own pricing drifts.
pricing-sync:
	cd tools/pricing-sync && GOWORK=off go run . $(CURDIR)
	gofmt -w pricing_data_gen.go

INTEGRATION_COMPOSE := integration/docker-compose.yml
INTEGRATION_HEALTH_URL := http://localhost:19081/v1/models
INTEGRATION_ALICE_KEY := sk-int-alice

# Overridable so the optional real-upstream smoke test (task 7,
# INTEGRATION_REAL=1) never hardcodes one operator's personal hostname as
# the only option: `INTEGRATION_REAL_BASEURL=https://other.example make
# integration` points it elsewhere. Rendered into
# integration/traefik/dynamic/dynamic.yml (git-ignored directory) from the
# checked-in dynamic.yml.tmpl by integration-up, below. Traefik's file
# provider watches that whole directory (traefik.yml's providers.file.directory),
# not the single rendered file — see traefik.yml's own comment for why.
INTEGRATION_REAL_BASEURL ?= https://llmgw.example.com

integration-up:
	mkdir -p integration/traefik/dynamic
	sed 's#__INTEGRATION_REAL_BASEURL__#$(INTEGRATION_REAL_BASEURL)#' integration/traefik/dynamic.yml.tmpl > integration/traefik/dynamic/dynamic.yml
	docker compose -f $(INTEGRATION_COMPOSE) up -d --build

integration-wait:
	@i=0; \
	until curl -sf -o /dev/null -H "Authorization: Bearer $(INTEGRATION_ALICE_KEY)" $(INTEGRATION_HEALTH_URL); do \
		i=$$((i+1)); \
		if [ $$i -ge 120 ]; then \
			echo "integration: traefik1 did not become healthy within 120s"; \
			docker compose -f $(INTEGRATION_COMPOSE) logs traefik1; \
			exit 1; \
		fi; \
		sleep 1; \
	done

integration-down:
	docker compose -f $(INTEGRATION_COMPOSE) down -v

# integration brings the stack up, waits for it to answer, runs the suite,
# then always tears the stack down — pass, fail, or a failure to even come
# up or become healthy — so a broken run never leaves a stray stack behind.
# integration-up/-wait run as recipe-body sub-makes (not prerequisites)
# specifically so a non-zero status from either one still falls through to
# integration-down instead of Make aborting before teardown ever runs.
integration:
	@$(MAKE) integration-up; status=$$?; \
	if [ $$status -eq 0 ]; then \
		$(MAKE) integration-wait; status=$$?; \
	fi; \
	if [ $$status -eq 0 ]; then \
		go -C integration test -tags integration -count=1 ./...; status=$$?; \
	fi; \
	$(MAKE) integration-down; \
	exit $$status

# integration-keep is the debugging variant: same run, but leaves the
# compose stack up afterward for inspecting logs/state by hand (including
# on a bring-up/health failure, deliberately — there's nothing to inspect
# post-mortem if it tore itself down). Tear it down yourself with
# `make integration-down` when done.
integration-keep: integration-up integration-wait
	go -C integration test -tags integration -count=1 ./...

# ---------------------------------------------------------------------------
# Standalone binary (deployment form B) — see README's "Deployment modes".
#
# The plugin form (A) needs none of these: Traefik interprets this
# repository's source directly. Everything below builds the compiled
# binary that runs BEHIND Traefik, which is what makes SSE stream
# incrementally (the Yaegi ResponseWriter erases http.Flusher; a compiled
# one does not).
#
# Built locally on purpose. .goreleaser.yaml stays at `builds: - skip:
# true` because its job is publishing the plugin SOURCE tarball for the
# Traefik catalog, and turning binary builds on there would put this on
# the GitHub Actions release path rather than the operator's machine.
# ---------------------------------------------------------------------------

# BINARY_VERSION stamps THIS binary's own -version output. It is NOT the
# plugin's pluginVersion, which is a const rewritten in source by
# workflow-prepare.sh at release time — `-ldflags -X` cannot write a const
# (verified: the build succeeds and the value never lands in the binary),
# so the two are stamped by different mechanisms and can disagree.
BINARY_VERSION ?= dev

build-binary:
	GOWORK=off CGO_ENABLED=0 go build -mod=vendor -trimpath \
		-ldflags "-s -w -X main.version=$(BINARY_VERSION)" \
		-o bin/llmgateway ./cmd/gateway

# IMAGE/PLATFORMS are overridable the same way INTEGRATION_REAL_BASEURL is,
# so this never hardcodes one operator's registry.
IMAGE ?= llmgateway:dev
PLATFORMS ?= linux/arm64,linux/amd64

# docker-build produces a multi-arch image. --push is required rather than
# optional: a multi-arch result CANNOT be --load-ed into the local docker
# image store, and without either flag buildx leaves the result in the
# build cache only — it looks like it succeeded and produces nothing
# usable. For a single-arch image you can actually run locally, use:
#   make docker-build PLATFORMS=linux/arm64 DOCKER_OUTPUT=--load
DOCKER_OUTPUT ?= --push

# --build-arg VERSION=$(BINARY_VERSION) (C3, review round 3, 2026-09):
# without it the Dockerfile's own ARG VERSION=dev default applies, and
# `llmgateway -version` inside the image always prints "dev" regardless
# of what BINARY_VERSION was set to for this invocation — the same
# BINARY_VERSION build-binary above stamps into bin/llmgateway, so both
# build paths agree on one version for one invocation.
docker-build:
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(BINARY_VERSION) -t $(IMAGE) $(DOCKER_OUTPUT) .

# run-local runs the binary against a config on this machine — the fastest
# way to exercise form (B) without a cluster. Point LLMGW_CONFIG at any
# config file; the checked-in test fixture is a real production config and
# makes a reasonable smoke target, though its `file:` secrets and
# in-cluster hostnames will not resolve off-cluster.
LLMGW_CONFIG ?= cmd/gateway/testdata/llmgw-config.yaml
LLMGW_LISTEN ?= :8080

run-local: build-binary
	./bin/llmgateway -config $(LLMGW_CONFIG) -listen $(LLMGW_LISTEN)
