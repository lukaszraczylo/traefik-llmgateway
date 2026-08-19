.PHONY: test lint yaegi-check integration

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
	@echo "yaegi-check: placeholder until Task 14"

integration:
	@echo "integration: placeholder until Task 15"
