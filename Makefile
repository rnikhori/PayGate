.PHONY: migrate-up migrate-down migrate-down-all migrate-version

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
