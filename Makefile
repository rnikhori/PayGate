.PHONY: help run \
	build-backend vet lint-backend test-backend \
	migrate-up migrate-down migrate-down-all migrate-version \
	install-root install-hooks format format-check \
	install-frontend-app lint-frontend-app typecheck-frontend-app test-frontend-app build-frontend-app \
	lint-frontend typecheck-frontend test-frontend build-frontend \
	lint test build

# Single entrypoint for the commands contributors and CI both run, so the
# two never drift apart (see .github/workflows/*-ci.yml, which call these
# same targets instead of re-typing the underlying commands). Every target
# here is safe to re-run: `go build`/`go test`/lint/typecheck are naturally
# idempotent, `npm ci` and `npm install` are deterministic reinstalls, and
# `migrate-up` only ever applies migrations that aren't already applied.
.DEFAULT_GOAL := help

FRONTEND_APPS := dashboard admin checkout-widget

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------

help: ## Show this help (default target)
	@echo "Usage: make <target> [APP=dashboard|admin|checkout-widget]"
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_.-]+:.*?## / {printf "  %-24s %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort

# ---------------------------------------------------------------------------
# Backend (see backend/go.mod, backend/cmd/{api,migrate})
# ---------------------------------------------------------------------------

run: ## Run the API server locally (reads backend/.env; see CONTRIBUTING.md)
	cd backend && go run ./cmd/api

vet: ## go vet the backend
	cd backend && go vet ./...

lint-backend: ## golangci-lint the backend (see .golangci.yml)
	cd backend && golangci-lint run ./...

build-backend: ## go build the backend
	cd backend && go build ./...

test-backend: ## go test the backend
	cd backend && go test ./...

# Schema migrations (see backend/migrations/README.md for the naming
# convention and backend/cmd/migrate for the runner). Point these at a
# non-local database by exporting DATABASE_URL before invoking make.
migrate-up: ## Apply all pending backend migrations
	cd backend && go run ./cmd/migrate up

migrate-down: ## Roll back one backend migration
	cd backend && go run ./cmd/migrate down

migrate-down-all: ## Roll back every backend migration
	cd backend && go run ./cmd/migrate down-all

migrate-version: ## Print the currently applied migration version
	cd backend && go run ./cmd/migrate version

# ---------------------------------------------------------------------------
# Root tooling (prettier for the pre-commit hook; see package.json)
# ---------------------------------------------------------------------------

install-root: ## Install root-level tooling deps (prettier)
	npm install

format: install-root ## Reformat frontend source with prettier
	npm run format

format-check: install-root ## Check frontend source is prettier-formatted
	npm run format:check

install-hooks: install-root ## Install the gofmt/prettier pre-commit hook
	git config core.hooksPath .githooks
	@echo "Pre-commit hook installed (git config core.hooksPath -> .githooks)."

# ---------------------------------------------------------------------------
# Frontend (frontend/{dashboard,admin,checkout-widget}, each its own app)
# ---------------------------------------------------------------------------
# Each *-app target takes APP=<name> and is the thing CI's per-app matrix job
# calls; the plain (non -app) targets below just loop over every app for the
# "do it all" local case (`make lint`, `make test`, ...).

install-frontend-app: ## Install one frontend app's deps (APP=<name>)
	@test -n "$(APP)" || (echo "APP is required, e.g. make install-frontend-app APP=dashboard" >&2 && exit 1)
	cd frontend/$(APP) && npm ci

lint-frontend-app: install-frontend-app ## Lint one frontend app (APP=<name>)
	cd frontend/$(APP) && npm run lint

typecheck-frontend-app: install-frontend-app ## Typecheck one frontend app (APP=<name>)
	cd frontend/$(APP) && npm run typecheck

test-frontend-app: install-frontend-app ## Test one frontend app (APP=<name>)
	cd frontend/$(APP) && npm run test

build-frontend-app: install-frontend-app ## Build one frontend app (APP=<name>)
	cd frontend/$(APP) && npm run build

lint-frontend: ## Lint every frontend app
	@for app in $(FRONTEND_APPS); do $(MAKE) lint-frontend-app APP=$$app; done

typecheck-frontend: ## Typecheck every frontend app
	@for app in $(FRONTEND_APPS); do $(MAKE) typecheck-frontend-app APP=$$app; done

test-frontend: ## Test every frontend app
	@for app in $(FRONTEND_APPS); do $(MAKE) test-frontend-app APP=$$app; done

build-frontend: ## Build every frontend app
	@for app in $(FRONTEND_APPS); do $(MAKE) build-frontend-app APP=$$app; done

# ---------------------------------------------------------------------------
# Aggregates (what CI and `make help`-reading contributors actually run)
# ---------------------------------------------------------------------------

lint: lint-backend lint-frontend format-check ## Lint backend + every frontend app + formatting

test: test-backend test-frontend ## Test backend + every frontend app

build: build-backend build-frontend ## Build backend + every frontend app
