package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IntegrityCheckTrigger records how one ledger integrity check run was
// invoked, matching the ledger_integrity_checks.trigger CHECK constraint
// (backend/migrations/*_create_ledger_integrity_checks).
type IntegrityCheckTrigger string

const (
	// ScheduledIntegrityCheck marks a run started by a job's own schedule
	// (see backend/internal/jobs' Scheduler) rather than on demand.
	ScheduledIntegrityCheck IntegrityCheckTrigger = "scheduled"
	// ManualIntegrityCheck marks a run started on demand -- this issue's
	// "and on demand" acceptance criterion -- by anything that calls
	// CheckIntegrity/RecordIntegrityCheck outside the regular schedule.
	ManualIntegrityCheck IntegrityCheckTrigger = "manual"
)

// Valid reports whether t is one of the trigger values the database allows.
func (t IntegrityCheckTrigger) Valid() bool {
	switch t {
	case ScheduledIntegrityCheck, ManualIntegrityCheck:
		return true
	default:
		return false
	}
}

// UnbalancedTransaction is one (transaction_ref, currency) group that
// CheckIntegrity found did not sum to zero on its own -- see CheckIntegrity's
// doc comment for why that should never happen against a healthy ledger.
type UnbalancedTransaction struct {
	TransactionRef string `json:"transaction_ref"`
	Currency       string `json:"currency"`
	Sum            int64  `json:"sum"`

	// AccountCodes lists, sorted and de-duplicated, every ledger_accounts.code
	// that posted at least one leg under TransactionRef -- which account(s)
	// a human investigating this failure needs to look at first.
	AccountCodes []string `json:"account_codes"`
}

// IntegrityReport is CheckIntegrity's result: a point-in-time snapshot of
// whether the entire ledger -- not just one account -- still honors the
// double-entry invariant that PostJournalEntry (post.go) and the
// journal_entries_balanced_transaction trigger
// (backend/migrations/*_create_journal_entries) are each already meant to
// guarantee on every single write.
type IntegrityReport struct {
	CheckedAt time.Time

	// GlobalSum is SUM(amount) across every journal_entries row that
	// exists, with no WHERE clause at all -- this issue's first
	// acceptance criterion ("sums every journal entry across the entire
	// ledger and asserts it equals zero"). It is always 0 for a healthy
	// ledger: every balanced transaction's own legs sum to zero, so the
	// sum of every transaction's sum is also zero.
	GlobalSum int64

	// UnbalancedTransactions is this issue's second acceptance criterion
	// ("spot-checks that no account's entries alone are unbalanced beyond
	// expected system accounts"). That wording is read here as: no single
	// transaction_ref's own legs, taken alone, may fail to sum to zero --
	// finer-grained than GlobalSum, since two separately-broken
	// transactions could cancel each other out in the global sum while
	// each is still individually unbalanced. Empty for a healthy ledger.
	//
	// "Beyond expected system accounts" is deliberately not read as "skip
	// the fixed system accounts (customer_suspense, gateway_fee_revenue,
	// settlement_clearing) when checking" -- every account, system or
	// per-merchant, must only ever appear in balanced transactions, with
	// no exception, and the existing journal_entries_balanced_transaction
	// trigger already enforces exactly that for every account alike. It is
	// read instead as a warning against the *other* natural misreading of
	// "unbalanced": an individual account's own running balance
	// (GetBalance) is expected to be nonzero for essentially every real
	// account -- that is what a balance *is* -- so flagging a nonzero
	// balance as "unbalanced" would make every system account (and every
	// merchant_payable account) a permanent false positive. This report
	// never looks at GetBalance at all; it only ever flags a
	// transaction_ref whose own legs fail to net to zero.
	UnbalancedTransactions []UnbalancedTransaction
}

// Passed reports whether this report found the ledger fully balanced: a
// zero GlobalSum and no UnbalancedTransactions. RecordIntegrityCheck stores
// exactly this value in ledger_integrity_checks.passed.
func (r IntegrityReport) Passed() bool {
	return r.GlobalSum == 0 && len(r.UnbalancedTransactions) == 0
}

// globalSumQuery is CheckIntegrity's first read: the ledger-wide sum this
// issue's first acceptance criterion describes.
const globalSumQuery = `SELECT COALESCE(SUM(amount), 0) FROM journal_entries`

// unbalancedTransactionsQuery is CheckIntegrity's second read: every
// transaction_ref whose own legs, grouped by currency, fail to sum to
// zero -- the same condition the journal_entries_balanced_transaction
// deferred trigger already rejects at write time (see
// backend/migrations/*_create_journal_entries), re-verified here from a
// read-only, independent query as this job's defense-in-depth audit rather
// than trusting that trigger was never bypassed (a future migration
// disabling it, or a privileged role writing outside PostJournalEntry,
// would otherwise go unnoticed indefinitely). account_codes is
// array_agg'd alongside the sum so a failing report can name exactly which
// ledger_accounts rows were party to each broken transaction_ref, without a
// second query per violation.
const unbalancedTransactionsQuery = `
	SELECT je.transaction_ref, je.currency, SUM(je.amount) AS total,
	       array_agg(DISTINCT la.code ORDER BY la.code) AS account_codes
	FROM journal_entries je
	JOIN ledger_accounts la ON la.id = je.account_id
	GROUP BY je.transaction_ref, je.currency
	HAVING SUM(je.amount) <> 0
	ORDER BY je.transaction_ref`

// CheckIntegrity is the nightly (or on-demand) ledger integrity job's only
// read of the ledger itself: two plain SELECTs, no write of any kind
// against journal_entries or ledger_accounts, satisfying this issue's "Job
// is read-only" acceptance criterion by construction -- there is no code
// path here that could ever attempt to "fix" the ledger, even by accident.
//
// Callers are expected to persist the returned IntegrityReport via
// RecordIntegrityCheck and, when it has not Passed(), alert -- see
// backend/internal/jobs, which wires both of those around this function on
// a schedule and on demand.
func CheckIntegrity(ctx context.Context, db StatementDB) (IntegrityReport, error) {
	report := IntegrityReport{CheckedAt: time.Now().UTC()}

	if err := db.QueryRowContext(ctx, globalSumQuery).Scan(&report.GlobalSum); err != nil {
		return IntegrityReport{}, fmt.Errorf("ledger: check integrity: global sum: %w", err)
	}

	rows, err := db.QueryContext(ctx, unbalancedTransactionsQuery)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("ledger: check integrity: unbalanced transactions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ut UnbalancedTransaction
		var codes accountCodeArray
		if err := rows.Scan(&ut.TransactionRef, &ut.Currency, &ut.Sum, &codes); err != nil {
			return IntegrityReport{}, fmt.Errorf("ledger: check integrity: scan unbalanced transaction: %w", err)
		}
		ut.AccountCodes = []string(codes)
		report.UnbalancedTransactions = append(report.UnbalancedTransactions, ut)
	}
	if err := rows.Err(); err != nil {
		return IntegrityReport{}, fmt.Errorf("ledger: check integrity: unbalanced transactions: %w", err)
	}

	return report, nil
}

// accountCodeArray scans a Postgres text[] value (as array_agg produces in
// unbalancedTransactionsQuery) into a []string, implementing database/sql's
// Scanner interface so CheckIntegrity can Scan it directly without
// depending on a third-party array type.
//
// This deliberately handles only the literal shape Postgres ever sends for
// an array of ledger_accounts.code values: "{code1,code2,...}" or "{}" for
// an empty array, with elements joined by a bare comma and no quoting. That
// narrower-than-general-purpose parsing is safe specifically because every
// element is itself already constrained by accountCodePattern (account.go)
// to lowercase letters, digits, '_', ':' and '-' -- never a comma, brace,
// double-quote, or backslash -- which are the only characters that would
// ever make Postgres quote or escape an array element. A general-purpose
// array-literal parser would be needed for arbitrary text[] values; this is
// not that, and must not be reused for a column without the same
// guarantee.
type accountCodeArray []string

func (a *accountCodeArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}

	var raw string
	switch v := src.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("ledger: scan account code array: unsupported type %T", src)
	}

	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return fmt.Errorf("ledger: scan account code array: malformed value %q", raw)
	}
	inner := raw[1 : len(raw)-1]
	if inner == "" {
		*a = accountCodeArray{}
		return nil
	}
	*a = strings.Split(inner, ",")
	return nil
}

// integrityReportDetails is the JSON shape stored in
// ledger_integrity_checks.details (backend/migrations/*_create_ledger_integrity_checks)
// -- just UnbalancedTransactions, since GlobalSum/CheckedAt/passed each
// already have their own column. Kept as its own type, rather than
// marshaling IntegrityReport directly, so that stored shape stays
// deliberate and stable even if IntegrityReport itself grows fields later
// that don't belong in this column.
type integrityReportDetails struct {
	UnbalancedTransactions []UnbalancedTransaction `json:"unbalanced_transactions,omitempty"`
}

// recordIntegrityCheckQuery inserts one ledger_integrity_checks row.
// RETURNING id lets RecordIntegrityCheck hand the new row's id back without
// a second round trip, the same pattern accounts_store.go's INSERT queries
// use.
const recordIntegrityCheckQuery = `
	INSERT INTO ledger_integrity_checks (checked_at, trigger, passed, global_sum, unbalanced_count, details)
	VALUES ($1, $2, $3, $4, $5, $6)
	RETURNING id`

// RecordIntegrityCheck persists report as one new ledger_integrity_checks
// row, tagged with trigger so a reader can tell a scheduled nightly run
// apart from one started on demand -- this issue's "Job result (pass/fail +
// sums) is persisted for audit/history" acceptance criterion. It is called
// for every run, not only failing ones: a long, unbroken history of passing
// checks is itself part of the audit trail.
//
// Every call inserts a new row; RecordIntegrityCheck never updates or
// deletes one, so this table only ever grows -- an append-only history of
// every run this job has ever made.
func RecordIntegrityCheck(ctx context.Context, db DB, trigger IntegrityCheckTrigger, report IntegrityReport) (int64, error) {
	if !trigger.Valid() {
		return 0, fmt.Errorf("ledger: record integrity check: invalid trigger %q", trigger)
	}

	details, err := json.Marshal(integrityReportDetails{UnbalancedTransactions: report.UnbalancedTransactions})
	if err != nil {
		return 0, fmt.Errorf("ledger: record integrity check: marshal details: %w", err)
	}

	var id int64
	row := db.QueryRowContext(ctx, recordIntegrityCheckQuery,
		report.CheckedAt, string(trigger), report.Passed(), report.GlobalSum, len(report.UnbalancedTransactions), string(details),
	)
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("ledger: record integrity check: %w", err)
	}
	return id, nil
}
