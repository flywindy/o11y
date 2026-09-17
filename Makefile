.PHONY: all test lint adr-check vuln examples bench cover fmt tidy clean help \
        tools sast sast-gosec sast-gosec-cred sast-semgrep sast-semgrep-test \
        sast-directives

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
# `go install` writes to $GOBIN when it is set, and only falls back to
# $GOPATH/bin when it is empty — honor that here too, or `make sast-gosec`'s
# executable check looks in the wrong place on any machine with GOBIN set.
# GOPATH itself can be a colon-separated list (`go help gopath`); `go install`
# then uses the first entry's bin dir, so appending /bin to the raw value
# would build a bogus path like "/a:/b/bin" on a multi-entry GOPATH.
GOBIN_DIR := $(shell $(GO) env GOBIN)
ifeq ($(strip $(GOBIN_DIR)),)
GOBIN_DIR := $(firstword $(subst :, ,$(shell $(GO) env GOPATH)))/bin
endif
TOOLS_GO_TOOLCHAIN := go1.25.13
GOSEC_VERSION      := v2.26.1
SEMGREP_VERSION    := 1.163.0

GOSEC := $(GOBIN_DIR)/gosec

# pipx installs semgrep into its own venv and exposes the entry point via
# PIPX_BIN_DIR (default ~/.local/bin), which `make tools` does not add to
# PATH — `pipx ensurepath` only edits shell rc files, so a fresh shell (or a
# CI job, before its own PATH tweak) may not have it yet. Resolve semgrep's
# location explicitly instead of assuming PATH: prefer whatever is already on
# PATH, else pipx's own reported bin dir, else the bare name (so the "not
# installed" message below still has something sensible to name).
SEMGREP := $(shell command -v semgrep 2>/dev/null)
ifeq ($(strip $(SEMGREP)),)
PIPX_BIN_DIR := $(shell command -v pipx >/dev/null 2>&1 && pipx environment --value PIPX_BIN_DIR 2>/dev/null)
ifneq ($(strip $(PIPX_BIN_DIR)),)
SEMGREP := $(PIPX_BIN_DIR)/semgrep
endif
endif
ifeq ($(strip $(SEMGREP)),)
SEMGREP := semgrep
endif

# gosec: -tests=true so it also catches issues planted in test code (mirrors
# sast-semgrep below — a fixture credential test asserts a value is absent
# from redacted output, it is not itself a live credential, but the shape is
# indistinguishable from a real leak without human review). .semgrep holds
# rule fixtures whose whole purpose is to contain deliberate violations, so it
# is excluded here the same way SEMGREP_FLAGS excludes it below.
#
# Only the credential half is wired into `sast` / CI, as sast-gosec-cred. The
# first run against this repo surfaced 12 pre-existing findings unrelated to
# the credential-literal gap sast-semgrep was added for (G115 int-overflow
# conversions, G404 weak RNG, G306 file perms, G102 bind-all). Those still
# need a human triage pass (fix vs justified #nosec) before the rest of gosec
# can block without either failing CI on unrelated findings or suppressing
# them un-reviewed. `make sast-gosec` stays runnable on demand until then.
#
# The G101 count in that tally was two. By the time a gate was put on it the
# class had reached eleven, all of them unannotated password-in-URL fixtures,
# because nothing was watching it — which is the argument for gating a class
# as soon as it is clean rather than waiting on the whole triage.
GOSEC_FLAGS := -quiet -severity medium -confidence low -tests=true \
               -exclude-generated -exclude-dir=.semgrep

# semgrep: only the repo-owned rules under .semgrep/ — no registry config, to
# keep this gate scoped to what it was added for (see
# .semgrep/hardcoded-credentials.yml) rather than opening a second, broader
# SAST-triage effort in the same change.
#
# Adding one would not close the gap the directives here name anyway.
# gosec.G101-1, the id every nosemgrep directive in this repo carries, is not
# in the public registry: probed in CI on 2026-09-17 over the three files
# holding credential fixtures, r/gosec.G101-1 resolved to zero rules (semgrep
# prints "Nothing to scan" and exits 0, so that gate would have passed having
# scanned nothing), while p/gosec, p/security-audit and p/golang ran 23, 30
# and 42 rules for zero findings, and every anonymous scan ends with "need
# more rules? semgrep login".
#
# It is served to logged-in orgs: the deployment that reported this tree runs
# Semgrep against an account, which is why the rule resolves for it and not
# here. Reproducing it would need that account's SEMGREP_APP_TOKEN in this
# repo's secrets, which is not a trade worth making for a gate — a scanning
# credential is exactly the kind of value the rule itself exists to keep out
# of a source tree. sast-directives enforces the convention instead.
#
# The directives do work there even though the rule cannot run here: semgrep
# matches a nosemgrep id by suffix, so "gosec.G101-1" silences the finding
# whatever namespace the full rule id carries in front of it.
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
	  found="$$(semgrep --version 2>/dev/null | head -n1)"; \
	  if [ "$$found" = "$(SEMGREP_VERSION)" ]; then \
	    echo "pipx not found; semgrep $(SEMGREP_VERSION) is already on PATH — skipping install"; \
	  else \
	    echo "pipx not found, and the semgrep on PATH is $${found:-unknown}, not the pinned $(SEMGREP_VERSION)." >&2; \
	    echo "Rule-test and exit-code behaviour differ between releases, so this is not accepted silently." >&2; \
	    echo "Install pipx, or: pip install --user semgrep==$(SEMGREP_VERSION)" >&2; \
	    exit 1; \
	  fi; \
	else \
	  echo "pipx not found and semgrep not on PATH — install pipx, or: pip install --user semgrep==$(SEMGREP_VERSION)" >&2; \
	  exit 1; \
	fi

# Run the blocking SAST gates plus the rule tests. Only gosec's credential
# rule is included; the rest of gosec is not — see the note on GOSEC_FLAGS
# above, and run `make sast-gosec` directly to see its findings. Every scan
# always runs (no fail-fast) so every category is reported in one pass.
sast: ## Run the blocking SAST gates: semgrep rules, their fixtures, credential directives, gosec G101
	@rc=0; s=PASS; t=PASS; d=PASS; c=PASS; \
	$(MAKE) --no-print-directory sast-semgrep-test   || { rc=1; t=FAIL; }; \
	$(MAKE) --no-print-directory sast-semgrep        || { rc=1; s=FAIL; }; \
	$(MAKE) --no-print-directory sast-directives     || { rc=1; d=FAIL; }; \
	$(MAKE) --no-print-directory sast-gosec-cred     || { rc=1; c=FAIL; }; \
	echo "==> SAST summary: semgrep=$$s directives=$$d gosec-G101=$$c rule-tests=$$t (full gosec not included — run 'make sast-gosec')"; \
	exit $$rc

sast-gosec: ## gosec: Go security static analysis (injection, weak crypto, unsafe code)
	@test -x "$(GOSEC)" || { echo "gosec not installed — run 'make tools'"; exit 1; }
	$(GOSEC) $(GOSEC_FLAGS) ./...

sast-semgrep: ## semgrep: repo-owned rules under .semgrep/
	@command -v "$(SEMGREP)" >/dev/null 2>&1 || { echo "semgrep not installed — run 'make tools' (needs pipx), or: pipx install semgrep==$(SEMGREP_VERSION)"; exit 1; }
	"$(SEMGREP)" scan $(SEMGREP_FLAGS) .

# Test the repo-owned rules against their fixtures, so a pattern edit that
# disables a rule fails here instead of silently passing every later scan.
#
# A rule file is tested when a Go fixture of the same basename sits beside it
# (.semgrep/hardcoded-credentials.yml -> .semgrep/hardcoded-credentials.go);
# the fixture must be a sibling because semgrep's test runner matches by
# basename and does not support a separate tests directory. Fixtures contain
# deliberate violations, which is why SEMGREP_FLAGS excludes .semgrep above.
# The hardcoded-credential half of gosec, split out so it can block while the
# rest stays untriaged (see GOSEC_FLAGS). G101 is the class this repo already
# writes directives for, and the class that had quietly grown from the 2
# findings GOSEC_FLAGS records to 11 with nothing gating it. Every remaining
# gosec rule stays out: wiring those in is still the separate triage effort.
sast-gosec-cred: ## gosec: hardcoded-credential findings only (G101), blocking
	@test -x "$(GOSEC)" || { echo "gosec not installed — run 'make tools'"; exit 1; }
	$(GOSEC) $(GOSEC_FLAGS) -include=G101 ./...

# gosec.G101-1, the id an external scan reports, runs nowhere here — it is not
# in the public semgrep registry at all (see SEMGREP_FLAGS). So the only thing
# this repo can enforce is that the id is named wherever a sibling scanner is
# suppressed, which is what the script checks. Its doc comment carries the full
# reasoning; a Go program rather than a shell recipe because the check needs to
# look at neighbouring lines, and because an awk pipeline that errors mid-run
# is exactly the silently-passing gate this whole target exists to prevent.
sast-directives: ## Check credential suppressions carry the external rule id
	$(GO) run ./scripts/check_credential_directives.go

sast-semgrep-test: ## Run the repo-owned semgrep rules against their fixtures
	@command -v "$(SEMGREP)" >/dev/null 2>&1 || { echo "semgrep not installed — run 'make tools' (needs pipx), or: pipx install semgrep==$(SEMGREP_VERSION)"; exit 1; }
	@rc=0; n=0; \
	for rule in .semgrep/*.yml; do \
	  fixture="$${rule%.yml}.go"; \
	  [ -f "$$fixture" ] || continue; \
	  n=$$((n+1)); \
	  echo "==> semgrep rule tests: $$rule"; \
	  ( cd .semgrep && "$(SEMGREP)" scan --test --metrics=off \
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
