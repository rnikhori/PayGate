// Package jobs hosts PayGate's scheduled / on-demand background jobs, each
// running some other package's read (or, one day, write) logic on a timer
// and recording what happened. Today it holds exactly one: the nightly
// ledger integrity check (ledger_integrity.go) built from
// backend/internal/ledger's read-only CheckIntegrity -- see that package's
// integrity.go for the check itself, and doc.go for why ledger cannot
// depend back on this package (or any other internal/* package) to run it.
package jobs
