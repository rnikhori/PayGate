// Package config loads PayGate's runtime configuration from environment
// variables, optionally seeded from a local ".env" file (see
// backend/.env.example). Load fails fast with a clear error if a required
// variable is missing, and the returned Config is passed explicitly into
// services — nothing outside this package should read os.Getenv for these
// settings, and nothing in this package is kept as mutable global state.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds PayGate API settings sourced from the environment.
type Config struct {
	// DatabaseURL is the full Postgres DSN (e.g.
	// "postgres://user:pass@host:5432/db?sslmode=disable"). Required.
	DatabaseURL string

	// WebhookSigningSecret is the HMAC-SHA256 secret used to sign outgoing
	// webhook payloads (X-PayGate-Signature). Required.
	WebhookSigningSecret string

	// HTTPPort is the port the API listens on. Defaults to "8080".
	HTTPPort string

	// LogLevel is the minimum log level to emit (debug|info|warn|error).
	// Defaults to "info".
	LogLevel string

	// LedgerIntegrityCheckInterval is how often the nightly ledger
	// integrity job (backend/internal/jobs.Scheduler, built on
	// backend/internal/ledger.CheckIntegrity) re-runs. Defaults to 24h
	// ("nightly") when unset; this is that job's "configurable" half of
	// its "Job runs on a schedule (configurable, default nightly)"
	// acceptance criterion -- the on-demand half is calling
	// jobs.LedgerIntegrityJob.RunOnce directly, which does not go through
	// this interval at all.
	LedgerIntegrityCheckInterval time.Duration
}

// requiredVar names an environment variable that Load must find set and
// non-empty, alongside where to store its value once found.
type requiredVar struct {
	name string
	dest *string
}

// Load reads Config from the environment, first loading a ".env" file from
// the current working directory if one is present (godotenv.Load returns an
// error when the file is absent; that error is intentionally ignored so
// deployments that supply real environment variables, with no ".env" file on
// disk, work unchanged). It fails fast with a single error naming every
// required variable that is missing or empty, rather than proceeding with a
// partially-configured Config.
func Load() (Config, error) {
	_ = godotenv.Load()

	cfg := Config{
		HTTPPort: getenvDefault("HTTP_PORT", "8080"),
		LogLevel: getenvDefault("LOG_LEVEL", "info"),
	}

	required := []requiredVar{
		{name: "DATABASE_URL", dest: &cfg.DatabaseURL},
		{name: "WEBHOOK_SIGNING_SECRET", dest: &cfg.WebhookSigningSecret},
	}

	var missing []string
	for _, rv := range required {
		v := os.Getenv(rv.name)
		if v == "" {
			missing = append(missing, rv.name)
			continue
		}
		*rv.dest = v
	}

	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: missing required environment variable(s): %s (see backend/.env.example)", strings.Join(missing, ", "))
	}

	interval, err := getenvDurationDefault("LEDGER_INTEGRITY_CHECK_INTERVAL", defaultLedgerIntegrityCheckInterval)
	if err != nil {
		return Config{}, err
	}
	cfg.LedgerIntegrityCheckInterval = interval

	return cfg, nil
}

// defaultLedgerIntegrityCheckInterval mirrors
// backend/internal/jobs.DefaultLedgerIntegrityCheckInterval (24h,
// "nightly") -- kept as its own literal here, rather than importing jobs
// from config, so configuration loading never depends on the job package it
// configures.
const defaultLedgerIntegrityCheckInterval = 24 * time.Hour

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getenvDurationDefault parses key as a time.Duration (e.g. "24h", "30m"),
// returning fallback when key is unset or empty. An explicitly set but
// unparseable value fails fast, the same way a missing required variable
// does, rather than silently falling back to fallback and masking a typo'd
// deployment setting.
func getenvDurationDefault(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid duration: %w", key, v, err)
	}
	return d, nil
}
