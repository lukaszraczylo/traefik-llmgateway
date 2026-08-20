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

integration-up:
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
# then always tears the stack down (pass or fail) so the exit code
# reflects the tests alone.
integration: integration-up integration-wait
	@go -C integration test -tags integration -count=1 ./...; status=$$?; \
	$(MAKE) integration-down; \
	exit $$status

# integration-keep is the debugging variant: same run, but leaves the
# compose stack up afterward for inspecting logs/state by hand. Tear it
# down yourself with `make integration-down` when done.
integration-keep: integration-up integration-wait
	go -C integration test -tags integration -count=1 ./...
