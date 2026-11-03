# Contributing to PayGate

This is the full path from a fresh clone to a running API, three running frontend apps, and
passing test suites. Follow it in order; nothing here assumes steps outside this doc.

## 1. Required tool versions

| Tool   | Version                                   | Check with           |
|--------|--------------------------------------------|-----------------------|
| Go     | 1.23+ (repo pins `go1.23.4` via toolchain) | `go version`          |
| Node   | 20.x                                       | `node --version`      |
| Docker | any recent version with Compose v2         | `docker compose version` |

Go and Node install the exact toolchain versions themselves (`go.mod`'s `toolchain` directive,
and `actions/setup-node@v4` in CI use Node 20) — the table above is the minimum your machine needs
before that kicks in. golangci-lint and prettier are pulled in by `go run`/`npm ci` below; you
don't need to install either by hand.

## 2. Clone

```
git clone https://github.com/rnikhori/PayGate.git
cd PayGate
```

## 3. Start Postgres

```
docker compose -f infra/docker-compose.yml up -d
```

This starts Postgres 16 on `localhost:5432` with user/password/db all `paygate`, backed by the
named volume `paygate_pg_data` (so data survives `docker compose down`; only `down -v` removes
it). To change the credentials or host port, copy `infra/.env.example` to `infra/.env` and edit it
before bringing the container up.

Confirm it's healthy before moving on:

```
docker compose -f infra/docker-compose.yml ps
```

## 4. Configure the backend

```
cp backend/.env.example backend/.env
```

The defaults in `backend/.env.example` already match the Postgres container from step 3, so for
local dev you only need to change `WEBHOOK_SIGNING_SECRET` if you want a non-placeholder value.
`backend/internal/config` loads this file via `godotenv` and fails fast if `DATABASE_URL` or
`WEBHOOK_SIGNING_SECRET` is unset — real environment variables (e.g. in CI) always take
precedence over the `.env` file.

## 5. Run migrations

```
make migrate-up
```

This runs `go run ./backend/cmd/migrate up`, which applies every pending file in
`backend/migrations/` (see `backend/migrations/README.md` for the naming convention) against the
`DATABASE_URL` from `backend/.env`. Point it at a different database by exporting `DATABASE_URL`
before calling `make` (this is how CI runs it against the ephemeral Postgres service container).

Other targets: `make migrate-down` (roll back one), `make migrate-down-all`, `make migrate-version`.

## 6. Start the API

```
make run
```

(equivalent to `cd backend && go run ./cmd/api`.)

It prints the loaded config (port, log level) and, as backlog issues wire up real HTTP handling,
will start serving on `HTTP_PORT` (default `8080`).

## 7. Start the frontend apps

There are three independent apps under `frontend/`: `dashboard`, `admin`, `checkout-widget`. Each
needs its own `npm install` (they are not a single workspace) and each runs on Vite's default dev
port, **5173** — see the troubleshooting section below if you want more than one running at once.

```
cd frontend/dashboard && npm install && npm run dev
cd frontend/admin && npm install && npm run dev
cd frontend/checkout-widget && npm install && npm run dev
```

## 8. Run the test suites

```
make lint
make test
```

`make lint` runs `golangci-lint` on the backend, `eslint` + a prettier formatting check on every
frontend app; `make test` runs `go test` on the backend and `vitest` on every frontend app. These
are the same targets `.github/workflows/backend-ci.yml` and `.github/workflows/frontend-ci.yml`
call, so a clean run locally means CI will pass too. Run `make help` to see every target,
including the single-app variants (e.g. `make test-frontend-app APP=dashboard`) CI uses under the
hood, and `make build` to build everything without running tests.

## 9. Install the formatting pre-commit hook (recommended)

```
make install-hooks
```

This installs the root `prettier` dependency and sets `core.hooksPath` to `.githooks`, so every
`git commit` runs `gofmt -l` / `prettier --list-different` on the files you staged and blocks the
commit if any would be reformatted. Fix with `gofmt -w <file>` / `npm run format` (repo root) and
re-stage, or skip once with `git commit --no-verify`.

## Troubleshooting

**Port conflicts**

- `:5432` (Postgres) — already in use (e.g. a system-wide Postgres) shows up as
  `docker compose up` failing to bind the port. Fix: stop the other service, or set
  `POSTGRES_PORT` in `infra/.env` and update `DATABASE_URL` in `backend/.env` to match.
- `:8080` (API) — set a different `HTTP_PORT` in `backend/.env` if something else on your machine
  already uses it.
- `:5173` (frontend dev servers) — all three frontend apps use Vite's default port, so running
  two of them at once will have the second one fail to bind (or auto-pick the next free port,
  depending on your Vite version's `strictPort` setting). Run only the app you're working on, or
  pass an explicit port: `npm run dev -- --port 5174`.

**Stale migrations**

- `go run ./cmd/migrate version` (via `make migrate-version`) prints the currently applied
  migration version — compare it against the newest file in `backend/migrations/` to see if
  you're behind.
- Pulled new commits that added migrations? Re-run `make migrate-up` — it only applies what's
  pending, so it's always safe to re-run.
- `migrate-up` erroring about a "dirty" database usually means a migration partially applied
  (e.g. the container was killed mid-run). Inspect the failing migration's SQL, fix the schema by
  hand if needed, then clear the dirty flag before retrying — see the
  [`golang-migrate` troubleshooting docs](https://github.com/golang-migrate/migrate) for the
  exact `force` invocation.
- When in doubt on local dev data you don't need to keep, the fastest reset is
  `docker compose -f infra/docker-compose.yml down -v && docker compose -f infra/docker-compose.yml up -d`
  followed by `make migrate-up` on a clean database.

**`npm ci` / lockfile errors**

Each frontend app has its own `package-lock.json`; run `npm install` (or `npm ci` to match CI
exactly) from inside that app's directory, not the repo root.

## 10. Code review checklist

A short list of repo-wide invariants that are easy to break in a PR and aren't all caught by
`make lint` / `make test`. Check these on every review, not just when touching the area they
name:

- **Ledger writes go through `ledger.PostJournalEntry` only.** No package outside
  `backend/internal/ledger` may `INSERT INTO journal_entries` (directly or via a raw query
  helper) — `PostJournalEntry` is the single place that checks the double-entry balance rule in
  Go before writing, and it's what every other service must call to move money. `make
  lint-ledger-writes` (part of `make lint-backend` / `make lint`) greps for that literal SQL
  pattern outside `backend/internal/ledger/` and fails the build if it finds one, but review the
  diff too: a grep can't see a query assembled from concatenated pieces.
