package jobs

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/rnikhori/paygate/internal/ledger"
)

// DefaultLedgerIntegrityCheckInterval is how often Scheduler runs when no
// interval is configured -- this issue's "configurable, default nightly"
// acceptance criterion's default half. See
// backend/internal/config.Config.LedgerIntegrityCheckInterval for the
// configurable half.
const DefaultLedgerIntegrityCheckInterval = 24 * time.Hour

// LedgerIntegrityAlerter is the seam LedgerIntegrityJob calls into when an
// integrity check does not pass -- this issue's "Job failure triggers the
// alerting path built in the Observability epic" acceptance criterion.
//
// That Observability epic (milestone "25 - Observability") has not been
// built yet: every internal/ package other than internal/ledger and this
// one is still just an unimplemented doc.go stub (see `ls backend/internal`
// as of this issue). There is, today, no paging/Slack/etc. path for this
// job to call into. LogAlerter below is this package's honest stand-in
// until that epic lands: a real, working implementation that never drops a
// failure silently (it logs it, at a severity an operator can already grep
// for), just not the paged-human one this acceptance criterion ultimately
// describes. Swapping in the real implementation, once the Observability
// epic exists, means constructing LedgerIntegrityJob with it instead of
// LogAlerter -- no change to CheckIntegrity, RunOnce, or Scheduler.
type LedgerIntegrityAlerter interface {
	AlertLedgerIntegrityFailure(ctx context.Context, report ledger.IntegrityReport) error
}

// LogAlerter is the LedgerIntegrityAlerter NewLedgerIntegrityJob uses until
// a caller has a real one to pass in -- see LedgerIntegrityAlerter's doc
// comment for why that is still a stand-in, not the finished Observability
// epic integration.
type LogAlerter struct{}

// AlertLedgerIntegrityFailure logs report through the standard library's
// log package, prefixed "ALERT" so a failing run is distinguishable in
// process logs from this package's other (non-alerting) log lines.
func (LogAlerter) AlertLedgerIntegrityFailure(_ context.Context, report ledger.IntegrityReport) error {
	log.Printf(
		"ALERT ledger integrity check failed: checked_at=%s global_sum=%d unbalanced_transactions=%d",
		report.CheckedAt.Format(time.RFC3339), report.GlobalSum, len(report.UnbalancedTransactions),
	)
	return nil
}

// LedgerIntegrityJob runs ledger.CheckIntegrity, persists its result via
// ledger.RecordIntegrityCheck, and -- only when the check did not pass --
// calls Alerter. Both fields are required; use NewLedgerIntegrityJob to
// fill in Alerter with LogAlerter when the caller has nothing more
// specific yet.
type LedgerIntegrityJob struct {
	DB      ledger.StatementDB
	Alerter LedgerIntegrityAlerter
}

// NewLedgerIntegrityJob returns a LedgerIntegrityJob wired to db and
// LogAlerter.
func NewLedgerIntegrityJob(db ledger.StatementDB) *LedgerIntegrityJob {
	return &LedgerIntegrityJob{DB: db, Alerter: LogAlerter{}}
}

// RunOnce runs the ledger integrity check exactly once for the given
// trigger -- this issue's "Job runs on a schedule ... and on demand"
// acceptance criterion's "on demand" half (Scheduler, below, is the
// schedule half); callers wanting an on-demand run (an ops script, or a
// future admin-panel button) call this directly with
// ledger.ManualIntegrityCheck.
//
// It always persists a ledger_integrity_checks row via
// ledger.RecordIntegrityCheck before returning, whether the check passed or
// not -- "Job result (pass/fail + sums) is persisted for audit/history" is
// true of every call -- and only then, if the check did not pass, calls
// j.Alerter.
//
// An error returned here means the check or its persistence itself failed
// (e.g. the database was unreachable) -- distinct from the check running
// successfully and finding the ledger unbalanced, which is not a Go error
// at all, only a report with Passed() == false that still gets persisted
// and alerted on.
func (j *LedgerIntegrityJob) RunOnce(ctx context.Context, trigger ledger.IntegrityCheckTrigger) (ledger.IntegrityReport, error) {
	report, err := ledger.CheckIntegrity(ctx, j.DB)
	if err != nil {
		return ledger.IntegrityReport{}, fmt.Errorf("jobs: ledger integrity check: %w", err)
	}

	if _, err := ledger.RecordIntegrityCheck(ctx, j.DB, trigger, report); err != nil {
		return report, fmt.Errorf("jobs: record ledger integrity check: %w", err)
	}

	if !report.Passed() {
		if err := j.Alerter.AlertLedgerIntegrityFailure(ctx, report); err != nil {
			return report, fmt.Errorf("jobs: alert on ledger integrity failure: %w", err)
		}
	}

	return report, nil
}

// Scheduler runs a LedgerIntegrityJob on a fixed interval until its context
// is canceled -- this issue's "Job runs on a schedule (configurable,
// default nightly)" acceptance criterion's schedule half.
type Scheduler struct {
	Job      *LedgerIntegrityJob
	Interval time.Duration
}

// NewScheduler returns a Scheduler for job, defaulting interval to
// DefaultLedgerIntegrityCheckInterval (nightly) when interval is <= 0 --
// e.g. when backend/internal/config.Config.LedgerIntegrityCheckInterval was
// left unset.
func NewScheduler(job *LedgerIntegrityJob, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = DefaultLedgerIntegrityCheckInterval
	}
	return &Scheduler{Job: job, Interval: interval}
}

// Run blocks, calling s.Job.RunOnce(ctx, ledger.ScheduledIntegrityCheck)
// once every s.Interval, until ctx is canceled -- intended to be started in
// its own goroutine by whatever wires the API process together (not yet
// done anywhere in cmd/api/main.go as of this issue; see that file's own
// doc comment).
//
// A RunOnce error is logged rather than returned or stopping the loop: one
// failed run (e.g. a transient database blip) must not silently end
// nightly checking until the process restarts -- the next tick tries
// again regardless.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Job.RunOnce(ctx, ledger.ScheduledIntegrityCheck); err != nil {
				log.Printf("jobs: scheduled ledger integrity check failed to run: %v", err)
			}
		}
	}
}
