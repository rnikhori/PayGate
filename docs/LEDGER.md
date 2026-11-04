# Ledger: Chart of Accounts

This is the reference doc for every `ledger_accounts` row the system posts against: what each one
is for, its type, which service posts to it, and when it's debited vs. credited. It exists so
anyone adding a new money-moving flow (settlements, disputes, payouts, ...) knows which existing
account to post against instead of inventing a new one, and so a reviewer can check a new flow's
entries against this list instead of against the Go source directly.

See `docs/ARCHITECTURE.md`'s ["Ledger model"](ARCHITECTURE.md#ledger-model) section for the
double-entry mechanics (`journal_entries`, `PostJournalEntry`, balanced-transaction invariant).
This doc is the account-level reference that section points to.

## Sign convention

Every `journal_entries.amount` is signed (`backend/internal/ledger/entry.go`'s `Entry.Amount`):

- **Positive amount = credit** to the account (increases its balance, `SUM(amount)`).
- **Negative amount = debit** to the account (decreases its balance).

This holds uniformly across every account below regardless of its `type` (asset / liability /
revenue) — it is not the "debits increase assets, credits increase liabilities" convention from
financial accounting, just this ledger's own signed-sum definition. A transaction's entries always
sum to zero per currency (enforced in Go by `ValidateBalancedTransaction` and in Postgres by the
`journal_entries_check_balanced_transaction` deferred trigger), so every credit somewhere is
matched by an equal debit (or set of debits) elsewhere.

## Accounts

| Code | Type | Seeded | Purpose |
|---|---|---|---|
| `customer_suspense` | liability | Fixed system account, seeded once by the `create_ledger_accounts` migration | Funds a customer has paid that PayGate is holding, not yet settled to any merchant |
| `gateway_fee_revenue` | revenue | Fixed system account, seeded once | PayGate's own earned processing-fee income (MDR) |
| `settlement_clearing` | asset | Fixed system account, seeded once | Funds in transit from a merchant's payable balance to their bank account |
| `merchant_payable:<merchant_id>` | liability | Created on demand, once per merchant, by `ledger.EnsureMerchantPayableAccount` | One merchant's available balance — what PayGate currently owes that merchant |

The fixed accounts are looked up by code via `ledger.GetAccountByCode` (never by a hard-coded id,
since ids are only assigned once by the seed migration and aren't guaranteed to match across
environments). `merchant_payable:<merchant_id>` accounts don't pre-exist for merchants that may
never sign up; `EnsureMerchantPayableAccount` upserts one idempotently the first time anything
needs to post against a given merchant.

Every account code is validated, both in Go (`ledger.ValidateAccountCode`) and by a matching
Postgres `CHECK` constraint on `ledger_accounts.code`, against one naming convention: a bare
lowercase snake_case token for a fixed system account, or that same token namespacing an entity id
after a single colon for a per-entity account (`merchant_payable:<merchant_id>` is the only
instance of that pattern today). A later per-entity account (e.g. a future per-merchant payout
account) should follow the same `<prefix>:<entity_id>` shape.

## Who posts what, and when

All of the below lives in `backend/internal/ledger` today — there is no `payments` or
`settlements` package yet (see each file's doc comments); those future packages will call these
`Post*` functions rather than posting to `journal_entries` directly, since `ledger.PostJournalEntry`
is the only write path into that table.

### Capture (`capture.go`: `PostCapture` / `PostCaptureTx`)

A payment capture moves the customer's gross payment out of suspense, net of PayGate's fee, into
the merchant's payable balance. Always exactly 3 legs:

| Account | Direction | Amount |
|---|---|---|
| `customer_suspense` | debit | `-grossAmount` |
| `merchant_payable:<merchant_id>` | credit | `+(grossAmount - fee)` |
| `gateway_fee_revenue` | credit | `+fee` |

`fee` is `FeeSchedule.Fee(grossAmount)` and must land strictly between 0 and `grossAmount` — a
capture that would yield a zero or full-amount fee is rejected rather than silently posting only
2 legs.

### Refund (`refund.go`: `PostRefund` / `PostRefundTx`)

A refund reverses some or all of a prior capture, proportionally reversing that capture's fee.
Always 2 legs, plus a 3rd only when the proportional fee reversal is non-zero:

| Account | Direction | Amount |
|---|---|---|
| `customer_suspense` | credit | `+refundAmount` |
| `merchant_payable:<merchant_id>` | debit | `-(refundAmount - feeReversal)` |
| `gateway_fee_revenue` | debit (omitted if 0) | `-feeReversal` |

`feeReversal` is `fee * refundAmount / grossAmount` (truncated toward zero), where `fee` and
`grossAmount` are read back from the original capture's own `journal_entries` rows rather than
passed in — `ResolveRefundContext` sums the capture's `customer_suspense` and
`gateway_fee_revenue` legs by `transaction_ref` to recover them. A refund is rejected
(`ErrRefundExceedsAvailableBalance`) if `refundAmount`, combined with every refund already posted
against this capture, would exceed the capture's original gross amount.

### Settlement clearing (`settlement.go`: `PostSettlementClearing`)

A settlement batch sweeps a merchant's eligible captured balance out of their payable account into
clearing, ahead of an eventual (simulated) bank transfer. Always 2 legs:

| Account | Direction | Amount |
|---|---|---|
| `merchant_payable:<merchant_id>` | debit | `-amount` |
| `settlement_clearing` | credit | `+amount` |

`amount` is whatever `ResolveSettlementAmount` finds newly eligible to clear as of the batch's
cutoff: every capture credited to `merchant_payable:<merchant_id>` before `cutoff`, minus whatever
a *previous* settlement batch already cleared for this same merchant (summed the same way a
refund's `AlreadyRefunded` is — by scanning every `settlement:<batchID>:<merchantID>`
`transaction_ref` ever posted for this merchant, across every batch, not just the current one).
Settlement does not yet post the simulated bank payout leg out of `settlement_clearing` itself —
that belongs to a later issue in the Settlements & Payouts epic, once a `Settlement`/`Payout`
schema exists to drive it.

### `transaction_ref` conventions

Each flow above namespaces its own `transaction_ref` so entries can be grouped and traced back
without a dedicated schema column pointing one transaction at another:

- Capture: caller-supplied (e.g. eventually a `Payment` id).
- Refund: `refund:<captureTransactionRef>:<refundRef>` (`RefundTransactionRef`).
- Settlement: `settlement:<batchID>:<merchantID>` (`SettlementTransactionRef`).

## Worked example: capture then refund

Merchant `mch_42` is captured ₹1,000.00 (100000 paise) at a 2% fee (200 basis points), then the
customer is refunded ₹400.00 (40000 paise) of it.

**1. Capture** — `transaction_ref = "pay_abc123"`, gross = 100000, fee = `100000 * 200 / 10000` = 2000,
net = 98000:

| transaction_ref | account | amount |
|---|---|---|
| `pay_abc123` | `customer_suspense` | -100000 |
| `pay_abc123` | `merchant_payable:mch_42` | +98000 |
| `pay_abc123` | `gateway_fee_revenue` | +2000 |

Balances after capture: `customer_suspense` = -100000, `merchant_payable:mch_42` = +98000,
`gateway_fee_revenue` = +2000. Sums to 0 across all three, as every transaction must.

**2. Refund** — `transaction_ref = "refund:pay_abc123:rfnd_1"`, refundAmount = 40000. Reading the
capture back: `grossAmount` = 100000, `fee` = 2000, `alreadyRefunded` = 0, so 40000 ≤ 100000 is
within the available balance. `feeReversal` = `2000 * 40000 / 100000` = 800, `netReversal` =
`40000 - 800` = 39200:

| transaction_ref | account | amount |
|---|---|---|
| `refund:pay_abc123:rfnd_1` | `customer_suspense` | +40000 |
| `refund:pay_abc123:rfnd_1` | `merchant_payable:mch_42` | -39200 |
| `refund:pay_abc123:rfnd_1` | `gateway_fee_revenue` | -800 |

Balances after refund: `customer_suspense` = -100000 + 40000 = **-60000** (PayGate is still
holding ₹600 of this customer's payment in suspense — the unrefunded, not-yet-settled portion),
`merchant_payable:mch_42` = 98000 - 39200 = **+58800** (the merchant's available balance, net of
fee, on the ₹600 that was never refunded), `gateway_fee_revenue` = 2000 - 800 = **+1200** (PayGate
keeps its 2% only on the ₹600 that stuck).

If `mch_42` is refunded another 60000 later against the same capture (`alreadyRefunded` would then
be 40000, leaving exactly 60000 of the 100000 gross still available), every balance above returns
to exactly 0 — the capture is fully reversed, no residue left in any of the three accounts.
