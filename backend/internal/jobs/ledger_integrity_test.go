package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rnikhori/paygate/internal/ledger"
)

// fakeRow/fakeRows below are this package's own minimal fakes for
// ledger.StatementDB, independent of internal/ledger's own test fakes
// (which are unexported to that package) -- just enough to drive
// LedgerIntegrityJob.RunOnce without a live database.
type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

type fakeRows struct{}

func (fakeRows) Next() bool        { return false }
func (fakeRows) Scan(...any) error { return nil }
func (fakeRows) Err() error        { return nil }
func (fakeRows) Close() error      { return nil }

// fakeLedgerDB implements ledger.StatementDB: its first QueryRowContext
// call (CheckIntegrity's global sum read) returns globalSum, its
// QueryContext call (CheckIntegrity's unbalanced-transactions read) always
// returns an empty fakeRows, and every QueryRowContext call *after* the
// first (RecordIntegrityCheck's INSERT ... RETURNING id) returns recordID.
type fakeLedgerDB struct {
	globalSum  int64
	recordID   int64
	recordErr  error
	rowCalls   int
	lastInsert []any
}

func (f *fakeLedgerDB) QueryRowContext(_ context.Context, _ string, args ...any) ledger.Row {
	f.rowCalls++
	if f.rowCalls == 1 {
		return fakeRow{scan: func(dest ...any) error {
			*dest[0].(*int64) = f.globalSum
			return nil
		}}
	}
	f.lastInsert = args
	return fakeRow{scan: func(dest ...any) error {
		if f.recordErr != nil {
			return f.recordErr
		}
		*dest[0].(*int64) = f.recordID
		return nil
	}}
}

func (f *fakeLedgerDB) QueryContext(_ context.Context, _ string, _ ...any) (ledger.Rows, error) {
	return fakeRows{}, nil
}

// fakeAlerter records every call made to it, so tests can assert
// RunOnce only alerts when the report did not pass.
type fakeAlerter struct {
	calls []ledger.IntegrityReport
	err   error
}

func (a *fakeAlerter) AlertLedgerIntegrityFailure(_ context.Context, report ledger.IntegrityReport) error {
	a.calls = append(a.calls, report)
	return a.err
}

func TestRunOnceRecordsAPassingCheckWithoutAlerting(t *testing.T) {
	db := &fakeLedgerDB{globalSum: 0, recordID: 1}
	alerter := &fakeAlerter{}
	job := &LedgerIntegrityJob{DB: db, Alerter: alerter}

	report, err := job.RunOnce(context.Background(), ledger.ScheduledIntegrityCheck)
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if !report.Passed() {
		t.Error("report.Passed() = false, want true for a zero global sum")
	}
	if len(alerter.calls) != 0 {
		t.Errorf("alerter called %d times, want 0 for a passing check", len(alerter.calls))
	}
	if db.rowCalls != 2 {
		t.Errorf("QueryRowContext called %d times, want 2 (global sum + record insert)", db.rowCalls)
	}
	if db.lastInsert[1] != string(ledger.ScheduledIntegrityCheck) {
		t.Errorf("recorded trigger = %v, want %q", db.lastInsert[1], ledger.ScheduledIntegrityCheck)
	}
}

func TestRunOnceAlertsAndRecordsOnFailingCheck(t *testing.T) {
	db := &fakeLedgerDB{globalSum: 777, recordID: 2}
	alerter := &fakeAlerter{}
	job := &LedgerIntegrityJob{DB: db, Alerter: alerter}

	report, err := job.RunOnce(context.Background(), ledger.ManualIntegrityCheck)
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Passed() {
		t.Error("report.Passed() = true, want false for a nonzero global sum")
	}
	if len(alerter.calls) != 1 {
		t.Fatalf("alerter called %d times, want 1 for a failing check", len(alerter.calls))
	}
	if alerter.calls[0].GlobalSum != 777 {
		t.Errorf("alerted report.GlobalSum = %d, want 777", alerter.calls[0].GlobalSum)
	}
	if db.lastInsert[1] != string(ledger.ManualIntegrityCheck) {
		t.Errorf("recorded trigger = %v, want %q", db.lastInsert[1], ledger.ManualIntegrityCheck)
	}
}

func TestRunOncePropagatesRecordError(t *testing.T) {
	db := &fakeLedgerDB{globalSum: 0, recordErr: errors.New("insert failed")}
	alerter := &fakeAlerter{}
	job := &LedgerIntegrityJob{DB: db, Alerter: alerter}

	if _, err := job.RunOnce(context.Background(), ledger.ManualIntegrityCheck); err == nil {
		t.Fatal("RunOnce() = nil error, want the propagated record error")
	}
	if len(alerter.calls) != 0 {
		t.Error("RunOnce alerted even though recording the result failed")
	}
}

func TestRunOncePropagatesAlerterError(t *testing.T) {
	db := &fakeLedgerDB{globalSum: 1, recordID: 3}
	alerter := &fakeAlerter{err: errors.New("pagerduty unreachable")}
	job := &LedgerIntegrityJob{DB: db, Alerter: alerter}

	if _, err := job.RunOnce(context.Background(), ledger.ManualIntegrityCheck); err == nil {
		t.Fatal("RunOnce() = nil error, want the propagated alerter error")
	}
}

func TestNewLedgerIntegrityJobDefaultsToLogAlerter(t *testing.T) {
	job := NewLedgerIntegrityJob(&fakeLedgerDB{})
	if _, ok := job.Alerter.(LogAlerter); !ok {
		t.Errorf("NewLedgerIntegrityJob's Alerter = %T, want LogAlerter", job.Alerter)
	}
}

func TestLogAlerterNeverErrors(t *testing.T) {
	if err := (LogAlerter{}).AlertLedgerIntegrityFailure(context.Background(), ledger.IntegrityReport{GlobalSum: 5}); err != nil {
		t.Errorf("LogAlerter.AlertLedgerIntegrityFailure returned error: %v", err)
	}
}

func TestNewSchedulerDefaultsIntervalWhenNonPositive(t *testing.T) {
	job := NewLedgerIntegrityJob(&fakeLedgerDB{})

	for _, interval := range []time.Duration{0, -time.Hour} {
		s := NewScheduler(job, interval)
		if s.Interval != DefaultLedgerIntegrityCheckInterval {
			t.Errorf("NewScheduler(job, %v).Interval = %v, want default %v", interval, s.Interval, DefaultLedgerIntegrityCheckInterval)
		}
	}
}

func TestNewSchedulerKeepsExplicitPositiveInterval(t *testing.T) {
	job := NewLedgerIntegrityJob(&fakeLedgerDB{})
	s := NewScheduler(job, 30*time.Minute)
	if s.Interval != 30*time.Minute {
		t.Errorf("NewScheduler interval = %v, want 30m", s.Interval)
	}
}

func TestSchedulerRunStopsOnContextCancelAndRunsOnEachTick(t *testing.T) {
	db := &fakeLedgerDB{globalSum: 0, recordID: 1}
	job := &LedgerIntegrityJob{DB: db, Alerter: &fakeAlerter{}}
	s := NewScheduler(job, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Scheduler.Run did not return after its context was canceled")
	}

	// At a 5ms interval over a 35ms window, at least one tick (and thus at
	// least one RunOnce -> QueryRowContext pair) must have fired.
	if db.rowCalls < 2 {
		t.Errorf("QueryRowContext called %d times, want at least 2 (one scheduled run)", db.rowCalls)
	}
}
