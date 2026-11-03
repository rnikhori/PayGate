// Command migrate is PayGate's thin wrapper around golang-migrate, driving the
// versioned SQL files in backend/migrations against the configured Postgres
// database. It exists so schema changes are applied the same way everywhere
// (local dev, CI, and eventually deploys) via `make migrate-up` / `make
// migrate-down` instead of hand-run psql scripts.
//
// Usage (run from backend/, which both `go run ./cmd/migrate` and the root
// Makefile targets do):
//
//	go run ./cmd/migrate up            # apply all pending migrations
//	go run ./cmd/migrate down          # roll back the most recent migration
//	go run ./cmd/migrate down-all       # roll back every migration
//	go run ./cmd/migrate version       # print the current schema version
//	go run ./cmd/migrate force VERSION  # mark the db at VERSION without running it (dirty-state recovery)
//
// Configuration is via environment variables, matching infra/.env.example:
//
//	DATABASE_URL     full Postgres DSN; overrides the POSTGRES_* vars below
//	POSTGRES_USER     default "paygate"
//	POSTGRES_PASSWORD default "paygate"
//	POSTGRES_DB       default "paygate"
//	POSTGRES_HOST     default "localhost"
//	POSTGRES_PORT     default "5432"
//	MIGRATIONS_PATH   default "migrations" (relative to the backend/ working dir)
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: migrate <up|down|down-all|version|force VERSION>")
	}

	cmd := args[0]
	if !isKnownCommand(cmd) {
		return fmt.Errorf("unknown command %q (want up|down|down-all|version|force)", cmd)
	}
	if cmd == "force" && len(args) < 2 {
		return errors.New("usage: migrate force VERSION")
	}

	m, err := newMigrator()
	if err != nil {
		return fmt.Errorf("connecting migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			fmt.Fprintln(os.Stderr, "migrate: closing source:", srcErr)
		}
		if dbErr != nil {
			fmt.Fprintln(os.Stderr, "migrate: closing database:", dbErr)
		}
	}()

	switch cmd {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-1)
	case "down-all":
		err = m.Down()
	case "version":
		var version uint
		var dirty bool
		version, dirty, err = m.Version()
		if err == nil {
			fmt.Printf("version=%d dirty=%t\n", version, dirty)
		}
	case "force":
		version, parseErr := strconv.Atoi(args[1])
		if parseErr != nil {
			return fmt.Errorf("invalid version %q: %w", args[1], parseErr)
		}
		err = m.Force(version)
	}

	if errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("migrate: no change")
		return nil
	}
	return err
}

func isKnownCommand(cmd string) bool {
	switch cmd {
	case "up", "down", "down-all", "version", "force":
		return true
	default:
		return false
	}
}

func newMigrator() (*migrate.Migrate, error) {
	migrationsPath := getenv("MIGRATIONS_PATH", "migrations")
	return migrate.New("file://"+migrationsPath, databaseURL())
}

// databaseURL builds the Postgres DSN from DATABASE_URL if set, otherwise
// from the same POSTGRES_* variables infra/docker-compose.yml and
// infra/.env.example use for local dev.
func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	user := getenv("POSTGRES_USER", "paygate")
	password := getenv("POSTGRES_PASSWORD", "paygate")
	db := getenv("POSTGRES_DB", "paygate")
	host := getenv("POSTGRES_HOST", "localhost")
	port := getenv("POSTGRES_PORT", "5432")

	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, db)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
