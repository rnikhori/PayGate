package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Role identifies the kind of caller InspectTransactions is being invoked
// on behalf of. The "Auth & API Key Management" epic (milestone 4) has not
// been built yet -- there is no session/RBAC system anywhere in this repo
// as of this issue -- so Role is a deliberately minimal stand-in, the same
// way backend/internal/jobs.LogAlerter stands in for the not-yet-built
// Observability epic: enough for this issue's "requires admin role, not
// accessible to merchants" acceptance criterion to be real and enforced
// today, without inventing the full auth system. Whatever eventually
// authenticates a request (the Admin Ops Panel's middleware) is expected to
// resolve a caller to one of these values before calling InspectTransactions
// -- this package has no opinion on how that happens, only on what it does
// once it has.
type Role string

const (
	// RoleAdmin is the only Role InspectTransactions ever allows through.
	RoleAdmin Role = "admin"
	// RoleMerchant marks an ordinary merchant-authenticated caller --
	// named explicitly, rather than leaving "not admin" as the only other
	// case, so a caller wiring this up has a real value to pass instead of
	// an empty string, and so ledger_admin_access_log rows for a denied
	// merchant lookup read as "merchant", not "".
	RoleMerchant Role = "merchant"
)

// Actor identifies who is calling InspectTransactions, for both the role
// gate and the audit log row it always writes.
type Actor struct {
	// ID is the caller's own identifier (e.g. an admin user id) -- opaque
	// to this package, stored as-is in ledger_admin_access_log.actor_id.
	ID string
	// Role is what InspectTransactions gates on: only RoleAdmin may read
	// raw journal entries through this path.
	Role Role
}

// ErrForbidden is the domain error InspectTransactions wraps into its
// returned error when actor.Role is not RoleAdmin, satisfying this issue's
// "Endpoint requires admin role, not accessible to merchants" acceptance
// criterion: a non-admin Actor never reaches the underlying query at all,
// it only ever reaches the audit log, as a denied attempt.
var ErrForbidden = errors.New("ledger: admin role required")

// InspectionEntry is one raw journal_entries row as InspectTransactions
// returns it: an Entry (see entry.go) plus the posting account's own code,
// not just its internal account_id -- this issue's "Response includes
// account code, not just internal account ID, for readability" acceptance
// criterion.
type InspectionEntry struct {
	Entry
	AccountCode string
}

// TransactionJournal is every InspectionEntry posted under one
// transaction_ref, in chronological order -- this issue's "Returns entries
// grouped by transaction_ref in chronological order" acceptance criterion.
type TransactionJournal struct {
	TransactionRef string
	Entries        []InspectionEntry
}

// InspectionDB is what InspectTransactions needs from a database handle:
// StatementDB's QueryContext, to read journal_entries/ledger_accounts, plus
// ExecContext, to write the ledger_admin_access_log row every call makes.
// WrapInspectionDB adapts a *sql.DB to it.
type InspectionDB interface {
	StatementDB
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// WrapInspectionDB adapts a *sql.DB for use as an InspectionDB, the same
// way WrapStatementDB (statement.go) adapts one to a StatementDB. sqlDB
// already satisfies InspectionDB once WrapStatementDB's QueryContext method
// exists: its embedded *sql.DB's own ExecContext already matches this
// interface's signature exactly, so no further adapter method is needed
// here.
func WrapInspectionDB(db *sql.DB) InspectionDB {
	return sqlDB{db}
}

// inspectionEntriesQuery lists every journal_entries row for the given
// transaction_ref values, each joined to its posting account's code.
// Ordering by transaction_ref, then created_at, then id gives
// InspectTransactions exactly the grouping (by transaction_ref) and
// per-group chronological order its acceptance criteria call for in one
// pass, with no further sorting needed in Go.
//
// $1 = the transaction_ref values to inspect, as a Postgres array literal
// built by inspectionRefsArrayLiteral -- see that function's doc comment
// for why a hand-built literal is used here instead of pq.Array/ANY($1).
const inspectionEntriesQuery = `
	SELECT je.id, je.transaction_ref, je.account_id, la.code, je.amount, je.currency, je.created_at
	FROM journal_entries je
	JOIN ledger_accounts la ON la.id = je.account_id
	WHERE je.transaction_ref = ANY($1::text[])
	ORDER BY je.transaction_ref, je.created_at, je.id`

// recordAdminAccessQuery inserts one ledger_admin_access_log row -- see
// that migration (backend/migrations/*_create_ledger_admin_access_log) for
// why this table exists and its append-only intent.
const recordAdminAccessQuery = `
	INSERT INTO ledger_admin_access_log (actor_id, actor_role, transaction_refs, allowed)
	VALUES ($1, $2, $3::jsonb, $4)`

// InspectTransactions returns every journal_entries row posted under each
// of transactionRefs, unformatted, for admin debugging of support tickets
// (this issue's own description). It is the one function in this package
// that reads raw ledger data without scoping to a single merchant's own
// account the way GetAccountStatement (statement.go) does, so unlike that
// function it enforces its own admin-only gate rather than trusting the
// caller already scoped the request correctly.
//
// Every call -- allowed or denied -- writes exactly one
// ledger_admin_access_log row recording actor, the transaction_refs asked
// for, and whether the lookup was allowed, before InspectTransactions
// returns: "Access to this endpoint is itself audit-logged" is true of
// every invocation, not only successful ones.
//
// actor.Role must be RoleAdmin or InspectTransactions returns an error
// wrapping ErrForbidden without ever querying journal_entries -- a
// RoleMerchant (or any other) Actor cannot read anything through this
// path, it can only generate a denied audit row.
func InspectTransactions(ctx context.Context, db InspectionDB, actor Actor, transactionRefs []string) ([]TransactionJournal, error) {
	refs := dedupeNonEmpty(transactionRefs)

	if actor.Role != RoleAdmin {
		// Best-effort: a failure to write the denied-access audit row must
		// not itself grant access, nor hide that a non-admin role was
		// rejected, so the rejection is returned either way -- but it is
		// still worth trying to record, and a logging outage shouldn't be
		// what a real admin sees when the actual problem is wrong
		// credentials.
		_ = recordAdminAccess(ctx, db, actor, refs, false)
		return nil, fmt.Errorf("ledger: inspect transactions: actor %q has role %q: %w", actor.ID, actor.Role, ErrForbidden)
	}

	if len(refs) == 0 {
		return nil, errors.New("ledger: inspect transactions: no transaction_ref given")
	}

	journals, err := queryInspectionEntries(ctx, db, refs)
	if err != nil {
		return nil, fmt.Errorf("ledger: inspect transactions for actor %q: %w", actor.ID, err)
	}

	// Unlike the denied path above, a successful disclosure of raw ledger
	// data that fails to log is not tolerated: the audit trail is as much
	// a required part of serving this data as the query itself, so a
	// logging failure here fails the whole call rather than silently
	// returning ungoverned data.
	if err := recordAdminAccess(ctx, db, actor, refs, true); err != nil {
		return nil, fmt.Errorf("ledger: inspect transactions for actor %q: record audit log: %w", actor.ID, err)
	}

	return journals, nil
}

// queryInspectionEntries runs inspectionEntriesQuery and groups its rows
// into one TransactionJournal per transaction_ref, in the order the query
// already returns them.
func queryInspectionEntries(ctx context.Context, db InspectionDB, refs []string) ([]TransactionJournal, error) {
	rows, err := db.QueryContext(ctx, inspectionEntriesQuery, inspectionRefsArrayLiteral(refs))
	if err != nil {
		return nil, fmt.Errorf("query journal entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var journals []TransactionJournal
	for rows.Next() {
		var e InspectionEntry
		if err := rows.Scan(&e.ID, &e.TransactionRef, &e.AccountID, &e.AccountCode, &e.Amount, &e.Currency, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan journal entry: %w", err)
		}

		if n := len(journals); n == 0 || journals[n-1].TransactionRef != e.TransactionRef {
			journals = append(journals, TransactionJournal{TransactionRef: e.TransactionRef})
		}
		last := &journals[len(journals)-1]
		last.Entries = append(last.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query journal entries: %w", err)
	}

	return journals, nil
}

// recordAdminAccess writes one ledger_admin_access_log row for a single
// InspectTransactions call.
func recordAdminAccess(ctx context.Context, db InspectionDB, actor Actor, refs []string, allowed bool) error {
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("marshal transaction_refs: %w", err)
	}

	if _, err := db.ExecContext(ctx, recordAdminAccessQuery, actor.ID, string(actor.Role), string(refsJSON), allowed); err != nil {
		return fmt.Errorf("insert ledger_admin_access_log: %w", err)
	}
	return nil
}

// dedupeNonEmpty returns refs with empty strings dropped and duplicates
// removed, preserving first-seen order -- callers (and the audit log) see
// one entry per distinct transaction_ref actually asked for, regardless of
// how many times (or in what order) a caller named it.
func dedupeNonEmpty(refs []string) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// inspectionRefsArrayLiteral renders refs as a Postgres array literal
// ('{"ref1","ref2"}') for inspectionEntriesQuery's $1::text[] parameter.
//
// A hand-built literal, rather than github.com/lib/pq's pq.Array helper, is
// used here because this repo never imports lib/pq directly in production
// code (see accountCodeArray in integrity.go, which hand-parses a text[]
// result for the same reason) -- only golang-migrate pulls it in, as an
// indirect dependency. Each ref is double-quoted with its own backslashes
// and double-quotes escaped, which is enough to safely embed any string
// Postgres's array literal syntax: unlike accountCodeArray's read-side
// parsing (safe only because ledger_accounts.code is already restricted to
// a narrow character set), this is the write side, so it must handle an
// arbitrary transaction_ref, not just one already known to be simple.
func inspectionRefsArrayLiteral(refs []string) string {
	var b []byte
	b = append(b, '{')
	for i, r := range refs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		for _, c := range []byte(r) {
			if c == '"' || c == '\\' {
				b = append(b, '\\')
			}
			b = append(b, c)
		}
		b = append(b, '"')
	}
	b = append(b, '}')
	return string(b)
}
