package main

import "testing"

func TestDatabaseURLDefaultsToLocalCompose(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("POSTGRES_USER", "")
	t.Setenv("POSTGRES_PASSWORD", "")
	t.Setenv("POSTGRES_DB", "")
	t.Setenv("POSTGRES_HOST", "")
	t.Setenv("POSTGRES_PORT", "")

	got := databaseURL()
	want := "postgres://paygate:paygate@localhost:5432/paygate?sslmode=disable"
	if got != want {
		t.Errorf("databaseURL() = %q, want %q", got, want)
	}
}

func TestDatabaseURLPrefersExplicitOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://custom/dsn")
	t.Setenv("POSTGRES_USER", "ignored")

	got := databaseURL()
	want := "postgres://custom/dsn"
	if got != want {
		t.Errorf("databaseURL() = %q, want %q", got, want)
	}
}

func TestDatabaseURLUsesPostgresEnvVars(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("POSTGRES_USER", "u")
	t.Setenv("POSTGRES_PASSWORD", "p")
	t.Setenv("POSTGRES_DB", "d")
	t.Setenv("POSTGRES_HOST", "db.internal")
	t.Setenv("POSTGRES_PORT", "6543")

	got := databaseURL()
	want := "postgres://u:p@db.internal:6543/d?sslmode=disable"
	if got != want {
		t.Errorf("databaseURL() = %q, want %q", got, want)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	// No live database is available in unit tests; an unknown command must
	// be rejected before main ever tries to connect, so this stays hermetic.
	t.Setenv("MIGRATIONS_PATH", "../../migrations")
	if err := run([]string{"sideways"}); err == nil {
		t.Error("run([\"sideways\"]) returned nil error, want an error for an unrecognized command")
	}
}

func TestRunRequiresACommand(t *testing.T) {
	if err := run(nil); err == nil {
		t.Error("run(nil) returned nil error, want a usage error")
	}
}
