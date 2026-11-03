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

## Status

Design phase. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the system design and
[`issues/`](issues/) for the backlog breakdown.
