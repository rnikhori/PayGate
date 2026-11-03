# PayGate — System Design

## 1. What this is

PayGate is a clone of a Razorpay-style payment gateway: a merchant integrates PayGate to accept
payments (cards, UPI, netbanking, wallets), PayGate settles collected funds to the merchant's bank
account on a schedule, and the merchant manages orders, refunds, disputes, and payouts through a
dashboard. All money movement outside PayGate's own ledger is simulated — there is no real bank or
card network behind this.

### Goals
- Model the real lifecycle of a payment: `created → authorized → captured → settled`, with
  `failed` / `refunded` / `disputed` branches.
- Support the four payment methods Razorpay is known for: Cards, UPI, Netbanking, Wallets.
- Correct, auditable money movement via a double-entry ledger — never mutate a balance directly.
- Idempotent APIs (safe retries), signed webhooks, scoped API keys — the actual hard parts of a
  payment gateway, not just CRUD screens.
- A merchant-facing dashboard and an internal admin/ops panel, same as a real gateway would need.

### Non-goals
- Real money movement, real bank/network integration, real PCI-DSS compliance.
- Multi-currency / cross-border (INR-only, matching Razorpay's core market).
- Mobile SDKs (web checkout only).

## 2. High-level architecture

```
                         ┌────────────────────┐
                         │   Merchant Server    │
                         │ (creates Orders via   │
                         │  server-side API key) │
                         └──────────┬───────────┘
                                    │ REST (API key auth)
                                    ▼
┌───────────┐   ┌─────────────────────────────────────┐   ┌──────────────────┐
│ Checkout   │   │              API Gateway              │   │  Admin / Ops     │
│ Widget (JS)│──▶│  authn · idempotency · rate limiting  │◀──┤  Panel (React)   │
└───────────┘   └───────────────┬───────────────────────┘   └──────────────────┘
                                 │
         ┌───────────────────────┼───────────────────────┐
         ▼                       ▼                       ▼
 ┌───────────────┐     ┌──────────────────┐     ┌──────────────────┐
 │ Orders &       │     │ Payment Method    │     │ Merchant /        │
 │ Payments       │────▶│ Adapters          │     │ Dashboard API     │
 │ Service        │     │ (Card/UPI/NB/     │     │ (React dashboard) │
 │                │     │  Wallet — mocked) │     └──────────────────┘
 └───────┬────────┘     └─────────┬────────┘
         │                        │
         ▼                        ▼
 ┌────────────────────────────────────────┐      ┌─────────────────┐
 │        Core Ledger (double-entry)        │─────▶│ Settlement &    │
 │  Postgres: immutable journal entries     │      │ Payout Engine   │
 └───────────────┬──────────────────────────┘      └─────────────────┘
                  │
                  ▼
      ┌───────────────────────┐        ┌───────────────────┐
      │ Webhook Dispatcher     │        │ Fraud/Risk Engine  │
      │ (signed, retried)      │        │ (rule-based)       │
      └───────────────────────┘        └───────────────────┘
```

Everything is event-driven off the ledger: a captured payment writes a journal entry, which emits
a domain event, which the webhook dispatcher and settlement engine both react to. Nothing reads
"current balance" from a mutable column — balances are always `SUM(journal entries)`.

## 3. Core domain model

| Entity | Purpose |
|---|---|
| `Merchant` | Onboarded business; owns API keys, bank account, settlement schedule |
| `Order` | What a customer is paying for (amount, currency, receipt, notes) |
| `Payment` | An attempt to pay an Order via one method; has its own state machine |
| `PaymentMethodDetail` | Polymorphic: card token, UPI VPA, netbanking bank code, wallet provider |
| `Refund` | Full/partial reversal of a captured Payment |
| `Dispute` | Chargeback/complaint raised against a Payment, with evidence workflow |
| `Settlement` | Batch of captured payments moved to a merchant's bank, minus fees |
| `Payout` | Merchant-initiated transfer out (vendor payouts, not customer-facing) |
| `JournalEntry` / `LedgerAccount` | Double-entry core: every money movement is two balanced rows |
| `Webhook` / `WebhookEvent` / `WebhookDelivery` | Merchant's webhook endpoints and delivery log |
| `ApiKey` | Scoped key pair (key_id/key_secret), test vs. live mode |
| `Plan` / `Subscription` / `Mandate` | Recurring billing |
| `PaymentLink` | Hosted payment page generated without an Order API call |

### Payment state machine
```
created ─▶ authorized ─▶ captured ─▶ settled
   │            │             │
   ▼            ▼             ▼
 failed       failed      refunded (full/partial)
                              │
                              ▼
                           disputed ─▶ lost / won
```

### Ledger model
Double-entry: every transaction posts ≥2 `JournalEntry` rows whose amounts sum to zero per
transaction, against `LedgerAccount`s such as `merchant_payable:<merchant_id>`,
`gateway_fee_revenue`, `customer_suspense`, `settlement_clearing`. A merchant's available balance
is a computed view (`SUM(journal_entries) WHERE account = merchant_payable:<id>`), never a stored
counter — this is what makes the system auditable and what most of the "Core Ledger" epic issues
are about.

`journal_entries` (`backend/migrations/*_create_journal_entries`) is append-only: every row has a
`transaction_ref` (groups the legs of one transaction), `account_id` (references
`ledger_accounts`), a signed `amount` (smallest currency unit, e.g. paise), `currency`
(ISO 4217), and `created_at`. A deferred constraint trigger checks, at commit, that every
`transaction_ref` has ≥2 rows summing to zero per currency; a second trigger unconditionally
rejects UPDATE/DELETE on the table, regardless of role, since Postgres lets a table's owner
bypass GRANT/REVOKE. A `paygate_app` DB role is created with only `SELECT`/`INSERT` on the
table — no role is ever granted `UPDATE`/`DELETE` — so that once the API's runtime connection
is switched to that role (a later deployment issue), the restriction holds without the trigger
too. Corrections are always a new, separately balanced transaction, never an edit to history.

`ledger.PostJournalEntry(ctx, db, transactionRef, entries)` (`backend/internal/ledger/post.go`) is
the single write path every other package must call to move money — no other package inserts
into `journal_entries` directly. It re-checks the balance rule in Go before issuing any SQL (so an
unbalanced call makes no partial write and returns a domain error,
`ledger.ErrUnbalancedTransaction`, distinct from a database error), then writes every entry for
one `transaction_ref` inside a single DB transaction and commits once, which is what lets the
deferred constraint trigger above verify the whole set.

`ledger.GetBalance(ctx, db, accountID)` (`backend/internal/ledger/balance.go`) is the read side of
the same model: `SELECT COALESCE(SUM(amount), 0) FROM journal_entries WHERE account_id = $1`,
using the index `journal_entries_account_id_idx` already creates. `ledger_accounts` has no
`balance` column; a materialized or cached balance remains explicitly out of scope until a later
issue deliberately adds one.

## 4. API design principles

- **Auth:** HTTP Basic with `key_id`/`key_secret` for server-to-server (mirrors Razorpay); session
  cookies + CSRF for the dashboard. Keys are scoped to `test` or `live` mode.
- **Idempotency:** every mutating POST accepts an `Idempotency-Key` header; replays with the same
  key + same body return the original response, replays with a different body are rejected.
- **Webhooks:** HMAC-SHA256 signed (`X-PayGate-Signature`), at-least-once delivery, exponential
  backoff retry, manual redelivery from the dashboard, per-merchant event subscription.
- **Errors:** consistent `{ error: { code, description, field } }` envelope, matching Razorpay's
  developer-experience pattern.
- **Versioning:** `/v1/...`, additive changes only within a version.

## 5. Payment method adapters (all simulated)

Each method is a pluggable adapter behind a common `PaymentMethodAdapter` interface
(`Authorize`, `Capture`, `Refund`, `HandleCallback`):

- **Cards:** tokenization, a mock 3DS/OTP redirect step, mock network response codes (approve/
  decline/insufficient funds) so retry and failure-handling logic has something real to do.
- **UPI:** Collect flow (request-to-pay on a VPA) and Intent flow (deep link), mock "switch" that
  simulates approve/decline/timeout.
- **Netbanking:** mock bank redirect page (pick a fake bank, "approve" or "cancel").
- **Wallets:** mock wallet provider balance + OTP step.

## 6. Repo structure (matches `backend/`, `frontend/`)

```
backend/
  cmd/api/                 entrypoint
  internal/ledger/         double-entry core — no business logic depends on anything else
  internal/orders/
  internal/payments/
  internal/methods/{card,upi,netbanking,wallet}/
  internal/settlements/
  internal/webhooks/
  internal/merchants/      onboarding, KYC, API keys
  internal/subscriptions/
  internal/fraud/
  internal/notifications/
  migrations/
frontend/
  dashboard/               merchant-facing React app
  admin/                   internal ops React app
  checkout-widget/         embeddable JS payment widget
```

## 7. Epics / milestones

The backlog (`issues/`) is organized into 28 epics, each a GitHub Milestone:

1. Project Setup & Infra
2. Core Ledger & Double-Entry Accounting
3. Merchant Onboarding & KYC
4. Auth & API Key Management
5. Orders API
6. Payments API (core engine)
7. Card Payments
8. UPI Payments
9. Netbanking Payments
10. Wallet Payments
11. Payment Links
12. Checkout Widget & Payment Pages
13. Subscriptions & Recurring Payments
14. Refunds
15. Disputes & Chargebacks
16. Settlements & Payouts
17. Webhooks & Event System
18. Fraud & Risk Rules Engine
19. Notifications (Email/SMS)
20. Merchant Dashboard (React)
21. Admin/Ops Panel (React)
22. Reporting & Analytics
23. Reconciliation
24. Rate Limiting & API Gateway
25. Observability (logs/metrics/tracing)
26. Security & Compliance (mock PCI controls, secrets mgmt)
27. Testing (unit/integration/e2e/load)
28. Documentation (API reference, developer portal)

Each issue in `issues/issues.json` carries: `epic`, `milestone`, `title`, `body` (with acceptance
criteria), `labels` (`module:*`, `type:feature|bug|infra|test|docs`, `priority:p0|p1|p2`).
