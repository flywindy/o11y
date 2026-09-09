.PHONY: all test lint adr-check vuln examples bench cover fmt tidy clean help \
        tools sast sast-gosec sast-semgrep sast-semgrep-test

# Force bash so the examples target can use process substitution / read -d ''.
# The default /bin/sh on Debian / Ubuntu is dash, which lacks both features.
SHELL    := /bin/bash

GO       ?= go
PKGS     := ./...
COVERAGE := coverage.out

# --- SAST tooling ------------------------------------------------------------
# Pinned tool versions. gosec/semgrep install via `go install` / pipx into
# $(GOBIN_DIR) (no go.mod impact). TOOLS_GO_TOOLCHAIN pins the toolchain used
# to *source-build* gosec so the install is reproducible regardless of the
# local Go: gosec < v2.26 fails to compile under Go 1.25.x.
GOBIN_DIR          := $(shell $(GO) env GOPATH)/bin
TOOLS_GO_TOOLCHAIN := go1.25.13
GOSEC_VERSION      := v2.26.1
SEMGREP_VERSION    := 1.163.0

GOSEC := $(GOBIN_DIR)/gosec

# gosec: -tests=true so it also catches issues planted in test code (mirrors
# sast-semgrep below — a fixture credential test asserts a value is absent
# from redacted output, it is not itself a live credential, but the shape is
# indistinguishable from a real leak without human review). .semgrep holds
# rule fixtures whose whole purpose is to contain deliberate violations, so it
# is excluded here the same way SEMGREP_FLAGS excludes it below.
#
# NOT wired into `sast` / CI yet: the first run against this repo surfaced 12
# pre-existing findings unrelated to the credential-literal gap sast-semgrep
# was added for (G115 int-overflow conversions, G404 weak RNG, G306 file
# perms, G102 bind-all, plus two real G101 password-in-URL fixtures). Those
# need a human triage pass (fix vs justified #nosec) before gosec can be a
# blocking gate without either failing CI on unrelated findings or suppressing
# them un-reviewed. `make sast-gosec` stays runnable on demand until then.
GOSEC_FLAGS := -quiet -severity medium -confidence low -tests=true \
               -exclude-generated -exclude-dir=.semgrep

# semgrep: only the repo-owned rules under .semgrep/ — no p/golang or
# p/security-audit registry config yet, to keep this gate scoped to what it
# was added for (see .semgrep/hardcoded-credentials.yml) rather than opening a
# second, broader SAST-triage effort in the same change.
SEMGREP_FLAGS := --error --severity=WARNING --severity=ERROR --metrics=off \
                 --exclude=.semgrep --config=.semgrep/

all: test lint vuln sast examples ## Run the full local check suite

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

test: ## Run unit tests with the race detector and coverage
	$(GO) test -race -covermode=atomic -coverprofile=$(COVERAGE) $(PKGS)

lint: adr-check ## Run ADR integration gate and golangci-lint (must be installed)
	golangci-lint run --timeout=5m $(PKGS)

adr-check: ## Run ADR 0008 integration policy gate
	$(GO) run ./scripts/check_integrations.go

vuln: ## Scan dependencies for known vulnerabilities
	govulncheck $(PKGS)

tools: ## Install pinned SAST tooling (gosec, semgrep)
	GOTOOLCHAIN=$(TOOLS_GO_TOOLCHAIN) $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	@if command -v pipx >/dev/null 2>&1; then \
	  pipx install --force semgrep==$(SEMGREP_VERSION) \
	    && pipx inject semgrep setuptools; \
	elif command -v semgrep >/dev/null 2>&1; then \
	  echo "pipx not found, but semgrep is already on PATH — skipping semgrep install"; \
	else \
	  echo "pipx not found and semgrep not on PATH — install pipx, or: pip install --user semgrep==$(SEMGREP_VERSION)" >&2; \
	  exit 1; \
	fi

# Run the blocking SAST scans (repo-owned semgrep rules) plus the rule tests.
# gosec is deliberately NOT included — see the note on GOSEC_FLAGS above; run
# `make sast-gosec` directly to see its findings. Both remaining scans always
# run (no fail-fast) so every category is reported in one pass.
sast: ## Run repo-owned semgrep rules + their fixture tests (gosec: run separately, see sast-gosec)
	@rc=0; s=PASS; t=PASS; \
	$(MAKE) --no-print-directory sast-semgrep-test || { rc=1; t=FAIL; }; \
	$(MAKE) --no-print-directory sast-semgrep      || { rc=1; s=FAIL; }; \
	echo "==> SAST summary: semgrep=$$s rule-tests=$$t (gosec not included — run 'make sast-gosec')"; \
	exit $$rc

sast-gosec: ## gosec: Go security static analysis (injection, weak crypto, unsafe code)
	@test -x "$(GOSEC)" || { echo "gosec not installed — run 'make tools'"; exit 1; }
	$(GOSEC) $(GOSEC_FLAGS) ./...

sast-semgrep: ## semgrep: repo-owned rules under .semgrep/
	@command -v semgrep >/dev/null 2>&1 || { echo "semgrep not installed — run 'make tools' (needs pipx), or: pipx install semgrep==$(SEMGREP_VERSION)"; exit 1; }
	semgrep scan $(SEMGREP_FLAGS) .

# Test the repo-owned rules against their fixtures, so a pattern edit that
# disables a rule fails here instead of silently passing every later scan.
#
# A rule file is tested when a Go fixture of the same basename sits beside it
# (.semgrep/hardcoded-credentials.yml -> .semgrep/hardcoded-credentials.go);
# the fixture must be a sibling because semgrep's test runner matches by
# basename and does not support a separate tests directory. Fixtures contain
# deliberate violations, which is why SEMGREP_FLAGS excludes .semgrep above.
sast-semgrep-test: ## Run the repo-owned semgrep rules against their fixtures
	@command -v semgrep >/dev/null 2>&1 || { echo "semgrep not installed — run 'make tools' (needs pipx), or: pipx install semgrep==$(SEMGREP_VERSION)"; exit 1; }
	@rc=0; n=0; \
	for rule in .semgrep/*.yml; do \
	  fixture="$${rule%.yml}.go"; \
	  [ -f "$$fixture" ] || continue; \
	  n=$$((n+1)); \
	  echo "==> semgrep rule tests: $$rule"; \
	  ( cd .semgrep && semgrep scan --test --metrics=off \
	      --config "$$(basename "$$rule")" "$$(basename "$$fixture")" ) || rc=1; \
	done; \
	if [ "$$n" -eq 0 ]; then echo "no semgrep rule fixtures found — expected at least one" >&2; exit 1; fi; \
	exit $$rc

examples: ## Build every example program
	@set -e; \
	find examples -name main.go -print0 | while IFS= read -r -d '' main; do \
		dir=$$(dirname "$$main"); \
		echo "build $$dir"; \
		$(GO) build -o /dev/null "./$$dir"; \
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
