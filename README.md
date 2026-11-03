# PayGate

A Razorpay-style payment gateway clone — built to understand and demonstrate how a real payment
processor is designed: order/payment lifecycle, multiple payment methods (cards, UPI, netbanking,
wallets), settlements, refunds, disputes, webhooks, and a merchant dashboard.

**This is an educational/portfolio clone.** It does not move real money, is not PCI-DSS certified,
and all "bank"/"network" integrations are simulated (mock UPI switch, mock card network, mock
netbanking bank pages) so the system can be run and demoed entirely locally.

## Stack

- **Backend:** Go (core ledger & payment engine, REST API)
- **Database:** PostgreSQL (system of record, double-entry ledger)
- **Frontend:** React + TypeScript (merchant dashboard, admin/ops panel, checkout widget)
- **Infra:** Docker Compose for local dev; see `infra/`

## Repo layout

```
backend/    Go services: API, ledger, payment-method adapters, webhooks, workers
frontend/   React apps: merchant dashboard, admin panel, embeddable checkout widget
docs/       Design docs (see docs/ARCHITECTURE.md)
infra/      docker-compose, migrations, local dev infra
issues/     Generated GitHub issue backlog (draft, reviewed before bulk-creation)
scripts/    Automation scripts (e.g. bulk-creating GitHub issues/labels/milestones)
```

## Local development

Start a local Postgres 16 instance (persisted in a named Docker volume):

```
docker compose -f infra/docker-compose.yml up -d
```

This brings up Postgres on `localhost:5432` with user/password/db all `paygate`. Data survives
`docker compose -f infra/docker-compose.yml down` / `up` since it's stored in the `paygate_pg_data`
named volume (only `docker compose down -v` removes it).

To override the credentials, db name, or host port, copy `infra/.env.example` to `infra/.env` and
edit it — `docker compose` picks up a `.env` file in the same directory as the compose file
automatically.

## Status

Design phase. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the system design and
[`issues/`](issues/) for the backlog breakdown.
