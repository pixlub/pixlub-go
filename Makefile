# Makefile for the pixlub Go SDK.
# Needs only Go and make. Run `make` or `make help` to list the targets.

GO    ?= go
PKG   := ./...
COVER := coverage.out

.DEFAULT_GOAL := help
.PHONY: help fmt fmt-check vet test race cover example live tidy tidy-check check clean

help: ## Show the available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format every Go file with gofmt
	gofmt -w .

fmt-check: ## Fail if a file is not gofmt-formatted (or does not parse)
	@out=$$(gofmt -l . 2>&1); status=$$?; \
	if [ $$status -ne 0 ] || [ -n "$$out" ]; then \
		echo "gofmt found problems (run make fmt):"; echo "$$out"; exit 1; \
	fi

vet: ## Run go vet
	$(GO) vet $(PKG)

test: ## Run the unit tests (no network needed)
	$(GO) test -count=1 $(PKG)

race: ## Run the unit tests with the race detector
	$(GO) test -count=1 -race $(PKG)

cover: ## Write a coverage profile to coverage.out and print the total
	$(GO) test -count=1 -coverprofile=$(COVER) $(PKG)
	$(GO) tool cover -func=$(COVER) | tail -1

example: ## Build the quickstart example
	$(GO) build -o /dev/null ./examples/quickstart

live: ## Run the live tests (needs PIXLUB_API_KEY and PIXLUB_BASE_URL)
	$(GO) test -count=1 -run Live -v $(PKG)

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

tidy-check: ## Fail if go mod tidy would change go.mod or go.sum
	$(GO) mod tidy -diff

check: fmt-check vet race example ## Everything CI needs: format, vet, race tests, example build

clean: ## Remove generated files
	rm -f $(COVER)
