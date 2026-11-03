# Migrations

Schema changes are versioned SQL files in this directory, run via
[`golang-migrate`](https://github.com/golang-migrate/migrate) through the
`backend/cmd/migrate` wrapper and the root `Makefile` targets:

```
make migrate-up      # apply all pending migrations
make migrate-down     # roll back the most recent migration
```

Local Postgres (via `docker compose -f infra/docker-compose.yml up -d`) must be
running first. See `infra/.env.example` for the connection settings these
targets use by default; set `DATABASE_URL` to point at a different database
(e.g. in CI).

## Naming convention

Every migration is a pair of files:

```
<timestamp>_<description>.up.sql
<timestamp>_<description>.down.sql
```

- `timestamp` — `YYYYMMDDHHMMSS`, UTC, the time the migration was authored.
  This keeps migrations ordered by creation time and avoids collisions
  between branches (compare to a sequential counter, which two branches can
  both claim).
- `description` — short, `snake_case`, describing what the migration does,
  e.g. `create_ledger_accounts` or `add_orders_idempotency_key`.
- `up.sql` — applies the change.
- `down.sql` — exactly reverses it. Every `up` must have a matching `down`;
  `golang-migrate` will not run one without the other present.

Example: `20261103000001_baseline.up.sql` / `20261103000001_baseline.down.sql`
is the empty baseline that proves the tooling works end-to-end.

Generate the timestamp with `date -u +%Y%m%d%H%M%S` so files sort correctly
regardless of which machine authors them.
