SHELL := /bin/bash
.DEFAULT_GOAL := help

GO       ?= go
BIN      := bin
LDFLAGS  := -X github.com/datopian/workgraph/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
            -X github.com/datopian/workgraph/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
            -X github.com/datopian/workgraph/internal/version.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: help bootstrap dev test check fmt vet lint build clean \
        web-install web-build web-dev verify-versions migrate-check sql-check infra-check \
        live-zones e2e

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------------------
## Local environment
## ---------------------------------------------------------------------------

bootstrap: ## Prepare a clean clone for development
	@bash scripts/bootstrap.sh

dev: ## Run the local stack (API, worker, web, PostgreSQL, fixture Beads)
	@bash scripts/dev.sh

## ---------------------------------------------------------------------------
## Quality gates — `make check` is what CI runs
## ---------------------------------------------------------------------------

check: fmt vet test verify-versions sql-check infra-check ## Run every gate CI runs

fmt: ## Fail if Go source is not gofmt-clean
	@out="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "gofmt OK"

vet: ## Run go vet
	@$(GO) vet ./...

test: ## Run unit tests with the race detector
	@$(GO) test -race ./...

build: ## Build all binaries into ./bin (without the web interface; see release)
	@mkdir -p $(BIN)
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/control-api ./cmd/control-api
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/worker      ./cmd/worker
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/agentd      ./cmd/agentd
	@$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/witness     ./cmd/witness
	@echo "built: $(BIN)/control-api $(BIN)/worker $(BIN)/agentd $(BIN)/witness"

build-linux: ## Cross-compile the node binaries for deployment (linux/amd64)
	@# The nodes are linux/amd64 and development happens on macOS, so a binary
	@# from `build` cannot be deployed. Ansible is pointed at this output.
	@mkdir -p $(BIN)/linux-amd64
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" \
		-o $(BIN)/linux-amd64/witness ./cmd/witness
	@echo "built: $(BIN)/linux-amd64/witness"

verify-versions: ## Check installed gt/bd/dolt against versions.lock
	@bash scripts/verify_versions.sh

sql-check: ## Basic structural checks on migrations
	@bash scripts/check_migrations.sh

live-zones: ## Smoke-check the Datopian production zones sharing this Cloudflare account
	@bash scripts/check_live_zones.sh

infra-check: ## Structural guards on the infrastructure security posture
	@python3 scripts/check_infra.py
	@command -v tofu >/dev/null 2>&1 && tofu fmt -recursive -check infra/tofu || echo "  (tofu not installed; formatting not checked)"

e2e: ## Playwright end-to-end tests (WP-F1)
	@echo "end-to-end suite lands with WP-F1"; exit 1

clean: ## Remove build output
	@rm -rf $(BIN) apps/web/dist coverage.out

## ---------------------------------------------------------------------------
## Web
## ---------------------------------------------------------------------------

web-install: ## Install web dependencies from the lockfile
	@cd apps/web && npm ci

web-build: ## Type-check, build, and embed the web application
	@cd apps/web && npm run build
	@# Copy the build into the Go binary's embed directory. Without this the
	@# binary starts cleanly, serves the API, and has no UI — which looks fine
	@# in logs and is only discovered by opening a browser.
	@rm -rf internal/webui/dist && cp -R apps/web/dist internal/webui/dist
	@# Restore the tracked placeholder. go:embed needs at least one file, and
	@# the rm above deletes it — a clean checkout then fails to compile with
	@# "no matching files found" while a working tree with build output is fine.
	@touch internal/webui/dist/.gitkeep
	@echo "embedded: $$(ls internal/webui/dist | tr '\n' ' ')"

web-dev: ## Run the web dev server
	@cd apps/web && npm run dev

.PHONY: release
release: web-build ## Build the deployable control API with the web interface embedded
	@# The only target that produces a binary suitable for deployment.
	@#
	@# `build` deliberately does NOT depend on web-build: that would make every
	@# Go build require Node and npm install, which broke the Go CI job — it has
	@# a Go toolchain and no node_modules, and has no reason to need them.
	@#
	@# The risk that separation reintroduces is a binary deployed without its
	@# interface. That is covered two ways: the API logs "no web interface
	@# embedded" at warning level on startup, and deployment uses this target.
	@CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/control-api ./cmd/control-api
	@echo "built with the web interface: $(BIN)/control-api"
