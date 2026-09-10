# Developer tasks for buzz-lakebox. Mirrors the hermetic checks in
# .github/workflows/ci.yml. Embedded helper generation is reproducible only
# with this exact toolchain; scripts/embedded-helpers.sh enforces the version.

GO              ?= go
GO_VERSION      := 1.26.8
VERSION         ?= dev
PROFILE         ?= DEFAULT
BINARY          := buzz-backend-databricks-lakebox
CMD             := ./cmd/$(BINARY)
MODULE          := github.com/IceRhymers/buzz-lakebox
LDFLAGS         := -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.DefaultProfile=$(PROFILE)
EMBEDDED_SCRIPT := ./scripts/embedded-helpers.sh

.DEFAULT_GOAL := help

.DELETE_ON_ERROR:

.PHONY: help build install symlink bzmux bzhttpmcp embedded-generate embedded-check embedded-smoke test vet lint fmt-check verify check clean

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Compatibility targets retained for contributors accustomed to `make bzmux`.
bzmux: ## Reproducibly regenerate the committed bzmux artifact with Go $(GO_VERSION)
	GO=$(GO) $(EMBEDDED_SCRIPT) generate bzmux

bzhttpmcp: ## Reproducibly regenerate the committed bzhttpmcp artifact with Go $(GO_VERSION)
	GO=$(GO) $(EMBEDDED_SCRIPT) generate bzhttpmcp

embedded-generate: ## Reproducibly regenerate both committed embedded helpers with Go $(GO_VERSION)
	GO=$(GO) $(EMBEDDED_SCRIPT) generate all

embedded-check: ## Rebuild both helpers twice and byte-compare with committed artifacts
	GO=$(GO) $(EMBEDDED_SCRIPT) check all

embedded-smoke: ## Execute offline smoke tests for both Linux/amd64 helpers
	GO=$(GO) $(EMBEDDED_SCRIPT) smoke all

build: ## Build the provider binary into the repo root
	$(GO) build -mod=readonly -ldflags '$(LDFLAGS)' -o $(BINARY) $(CMD)

install: ## Install into GOBIN; PROFILE=<name> bakes in a default Databricks profile
	$(GO) install -mod=readonly -ldflags '$(LDFLAGS)' $(CMD)

# ~/.local/bin, NOT /usr/local/bin: a GUI-launched Buzz Desktop inherits
# launchd's minimal PATH and augments provider discovery with only its own
# app bundle dir and ~/.local/bin (block/buzz
# desktop/src-tauri/src/managed_agents/backend.rs) — /usr/local/bin is
# never scanned in the GUI-launched case.
SYMLINK_DIR ?= $(HOME)/.local/bin

symlink: ## Symlink the installed binary into SYMLINK_DIR so Buzz Desktop finds it
	@gobin="$$($(GO) env GOBIN)"; [ -n "$$gobin" ] || gobin="$$($(GO) env GOPATH)/bin"; \
	src="$$gobin/$(BINARY)"; dest="$(SYMLINK_DIR)/$(BINARY)"; \
	[ -x "$$src" ] || { echo "$$src not found; run 'make install' first"; exit 1; }; \
	mkdir -p "$(SYMLINK_DIR)"; \
	if [ -e "$$dest" ] || [ -L "$$dest" ]; then \
		echo "$$dest already exists; leaving it in place"; \
	else \
		ln -s "$$src" "$$dest" && echo "linked $$dest -> $$src"; \
	fi

test: ## Run hermetic unit tests with the race detector
	env -u DATABRICKS_CONFIG_PROFILE -u DATABRICKS_HOST -u DATABRICKS_TOKEN $(GO) test -mod=readonly ./... -race

vet: ## Run go vet
	env -u DATABRICKS_CONFIG_PROFILE -u DATABRICKS_HOST -u DATABRICKS_TOKEN $(GO) vet ./...

lint: ## Run golangci-lint (CI pins v2.13.2)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found; install it from https://golangci-lint.run (CI uses v2.13.2)"; exit 1; }
	golangci-lint run ./...

fmt-check: ## Check gofmt formatting
	test -z "$$(gofmt -l .)"

verify: fmt-check vet lint test embedded-check embedded-smoke ## Run full hermetic verification

check: verify ## Alias for full hermetic verification

clean: ## Remove build artifacts
	rm -f $(BINARY)
	rm -rf dist
	$(GO) clean
