# Serialize targets even under `make -j`: the integration targets are
# stateful (one docker compose stack, one set of host ports) and running
# two of them concurrently would corrupt each other's containers.
.NOTPARALLEL:

.PHONY: test lint yaegi-check integration integration-keep integration-up integration-wait integration-down

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

yaegi-check:
	cd tools/yaegi-check && GOWORK=off go run . $(CURDIR)

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
