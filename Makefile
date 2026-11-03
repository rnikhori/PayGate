.PHONY: migrate-up migrate-down migrate-down-all migrate-version install-hooks

# Schema migrations (see backend/migrations/README.md for the naming
# convention and backend/cmd/migrate for the runner). Point these at a
# non-local database by exporting DATABASE_URL before invoking make.
migrate-up:
	cd backend && go run ./cmd/migrate up

migrate-down:
	cd backend && go run ./cmd/migrate down

migrate-down-all:
	cd backend && go run ./cmd/migrate down-all

migrate-version:
	cd backend && go run ./cmd/migrate version

# One-line pre-commit hook setup (see .githooks/pre-commit). Installs the
# root-level prettier used for the frontend formatting check, then points
# git at .githooks so the hook actually runs on `git commit`.
install-hooks:
	npm install
	git config core.hooksPath .githooks
	@echo "Pre-commit hook installed (git config core.hooksPath -> .githooks)."
