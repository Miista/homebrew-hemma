# hemma — everyday commands. `make` (or `make help`) lists them.
#
# The release workflow builds and publishes the packages; nothing here deploys to a real host.
# The fixture targets only ever touch the SYNTHETIC test stack under hack/ui-fixture.

GO      ?= go
COMPOSE ?= docker compose -f compose.yml
GOFILES  = cmd internal tools

.DEFAULT_GOAL := help
.PHONY: help build hemma check fmt fmt-check vet test test-fast test-race \
        snapshots snapshots-update man completions \
        ui-binary ui-image fixture-redeploy fixture-logs fixture-verify fixture-import fixture-import-authelia fixture-ps fixture-reset clean

help: ## list the targets
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---------- build ----------
build: ## compile everything
	$(GO) build ./...

hemma: ## build the CLI binary at ./hemma (gitignored)
	$(GO) build -o hemma .

# ---------- quality gates ----------
check: fmt-check vet test ## everything CI-ish: formatting, vet, the full test suite

fmt: ## gofmt -w
	gofmt -w $(GOFILES)

fmt-check: ## fail if anything is not gofmt-clean
	@out="$$(gofmt -l $(GOFILES))"; if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) vet ./...

test: ## full test suite (includes the ~40s CLI snapshot tests)
	$(GO) test ./... -count=1

test-fast: ## everything except the CLI snapshot tests
	$(GO) test $$($(GO) list ./... | grep -v 'hemma/internal/cli$$') -count=1

test-race: ## the concurrency-sensitive packages under the race detector
	$(GO) test -race ./cmd/hemma-ui/ ./internal/cli/kernel/ -count=1

snapshots: ## only the CLI snapshot tests (every command, against the anonymized fixture)
	$(GO) test ./internal/cli/ -run TestSnapshots -count=1

snapshots-update: ## regenerate the snapshot goldens — then REVIEW `git diff internal/cli/testdata`
	$(GO) test ./internal/cli/ -run TestSnapshots -update -count=1
	@echo; echo "Goldens rewritten. Review the diff before committing:"; git --no-pager diff --stat -- internal/cli/testdata | tail -5

# ---------- generated files (gitignored; the release workflow regenerates them) ----------
man: ## regenerate man/hemma.1.gz from the CLI help
	$(GO) run ./tools/genman

completions: ## regenerate shell completions into completions/
	$(GO) run ./tools/gencompletions -dir completions

# ---------- hemma-ui ----------
# The image never compiles Go: the binary is cross-compiled here (the Mac is the build machine) and Dockerfile.ui just copies it in.
# UI_ARCH is the target machine's: arm64 for a Raspberry Pi and for Colima / Docker Desktop on Apple silicon; amd64 for an x86 host.
UI_ARCH ?= arm64
ui-binary: ## cross-compile hemma-ui for Linux into dist/ (UI_ARCH=arm64|amd64)
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(UI_ARCH) $(GO) build -trimpath -ldflags="-s -w" -o dist/hemma-ui ./cmd/hemma-ui

ui-image: ui-binary ## build the hemma-ui image (tag: hemma-ui) from the cross-compiled binary
	docker build --platform linux/$(UI_ARCH) -f Dockerfile.ui -t hemma-ui .

# The fixture is synthetic. It listens on loopback only, unless you opt in by putting
# FIXTURE_BIND=<address> in hack/ui-fixture/.env (gitignored) to reach it from another device.
# FIXTURE_BIND=auto resolves the address for the active Docker runtime (Colima: the VM's address;
# Docker Desktop: the Mac's LAN address — see hack/ui-fixture/bind-address.sh), so a DHCP change does
# not silently strand a pinned one. (Never a default: the UI container has the Docker socket.)
-include hack/ui-fixture/.env
ifeq ($(FIXTURE_BIND),auto)
  FIXTURE_BIND := $(shell hack/ui-fixture/bind-address.sh)
  ifeq ($(FIXTURE_BIND),)
    $(error FIXTURE_BIND=auto but no address could be determined — set one explicitly in hack/ui-fixture/.env)
  endif
endif

fixture-redeploy: ui-binary ## rebuild + recreate ONLY the hemma-ui container; repo data and your account are kept
	cd hack/ui-fixture && FIXTURE_BIND=$(FIXTURE_BIND) $(COMPOSE) up -d --build --no-deps hemma-ui
	@echo "UI: http://$(if $(FIXTURE_BIND),$(FIXTURE_BIND),127.0.0.1):18080"

fixture-logs: ## follow the hemma-ui container's log
	cd hack/ui-fixture && $(COMPOSE) logs -f --tail=50 hemma-ui

fixture-verify: ## read-only: did the last Deploy leave the running stack matching the repo?
	cd hack/ui-fixture && ./verify.sh

fixture-import: ## put a NEUTRALIZED copy of a real services.yaml into the fixture: SRC=<path>. Backs up first
	@[ -n "$(SRC)" ] || { echo "usage: make fixture-import SRC=<path to a services.yaml>"; exit 2; }
	hack/ui-fixture/import.sh "$(SRC)"

fixture-import-authelia: ## give the fixture a SECRET-FREE Authelia config from a real one piped on stdin; SERVICES=<real services.yaml>
	@[ -n "$(SERVICES)" ] || { echo "usage: ssh <host> 'cat …/configuration.yml' | make fixture-import-authelia SERVICES=<real services.yaml>"; exit 2; }
	hack/ui-fixture/import-authelia.sh "$(SERVICES)"

fixture-ps: ## fixture containers and their status
	cd hack/ui-fixture && $(COMPOSE) ps

fixture-reset: ui-binary ## DESTRUCTIVE: wipe and rebuild the fixture (repo + account). Needs CONFIRM=yes
	@[ "$(CONFIRM)" = "yes" ] || { echo "This deletes the fixture repo and the UI account. Re-run with CONFIRM=yes."; exit 1; }
	cd hack/ui-fixture && $(COMPOSE) down -v && ./setup.sh && $(COMPOSE) up -d --build
	@echo; echo "Open the UI now and create the account: the first visitor claims it."

clean: ## remove the built ./hemma binary and dist/
	rm -rf hemma dist
