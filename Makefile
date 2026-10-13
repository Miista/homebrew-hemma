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
        ui-image fixture-redeploy fixture-logs fixture-verify fixture-ps fixture-reset clean

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
ui-image: ## build the hemma-ui image (tag: hemma-ui)
	docker build -f Dockerfile.ui -t hemma-ui .

# The fixture is synthetic. Put FIXTURE_BIND=<address> in hack/ui-fixture/.env (gitignored) to reach it
# from another device; otherwise it listens on loopback only.
fixture-redeploy: ## rebuild + recreate ONLY the hemma-ui container; repo data and your account are kept
	cd hack/ui-fixture && $(COMPOSE) up -d --build --no-deps hemma-ui

fixture-logs: ## follow the hemma-ui container's log
	cd hack/ui-fixture && $(COMPOSE) logs -f --tail=50 hemma-ui

fixture-verify: ## read-only: did the last Deploy leave the running stack matching the repo?
	cd hack/ui-fixture && ./verify.sh

fixture-ps: ## fixture containers and their status
	cd hack/ui-fixture && $(COMPOSE) ps

fixture-reset: ## DESTRUCTIVE: wipe and rebuild the fixture (repo + account). Needs CONFIRM=yes
	@[ "$(CONFIRM)" = "yes" ] || { echo "This deletes the fixture repo and the UI account. Re-run with CONFIRM=yes."; exit 1; }
	cd hack/ui-fixture && $(COMPOSE) down -v && ./setup.sh && $(COMPOSE) up -d --build
	@echo; echo "Open the UI now and create the account: the first visitor claims it."

clean: ## remove the built ./hemma binary
	rm -f hemma
