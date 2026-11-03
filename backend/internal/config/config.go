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

	return cfg, nil
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
