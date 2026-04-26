.PHONY: all test lint vuln examples bench cover fmt tidy clean help

GO       ?= go
PKGS     := ./...
COVERAGE := coverage.out

all: test lint vuln examples ## Run the full local check suite

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

test: ## Run unit tests with the race detector and coverage
	$(GO) test -race -covermode=atomic -coverprofile=$(COVERAGE) $(PKGS)

lint: ## Run golangci-lint (must be installed)
	golangci-lint run --timeout=5m $(PKGS)

vuln: ## Scan dependencies for known vulnerabilities
	govulncheck $(PKGS)

examples: ## Build every example program
	@set -e; \
	for main in $$(find examples -name main.go); do \
		dir=$$(dirname $$main); \
		echo "build $$dir"; \
		$(GO) build -o /dev/null ./$$dir; \
	done

bench: ## Run benchmarks (allocations included)
	$(GO) test -run=^$$ -bench=. -benchmem $(PKGS)

cover: test ## Open the coverage report in a browser
	$(GO) tool cover -html=$(COVERAGE)

fmt: ## Format the codebase
	$(GO) fmt $(PKGS)

tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

clean: ## Remove generated files
	rm -f $(COVERAGE)
