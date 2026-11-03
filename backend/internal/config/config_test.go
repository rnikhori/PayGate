package config

import "testing"

func TestLoadFailsFastWhenAllRequiredVarsMissing(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error, want an error when required vars are missing")
	}
}

func TestLoadFailsFastWhenOneRequiredVarMissing(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/paygate?sslmode=disable")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error, want an error when WEBHOOK_SIGNING_SECRET is missing")
	}
}

func TestLoadAppliesDefaultsWhenOptionalVarsUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/paygate?sslmode=disable")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "s3cret")
	t.Setenv("HTTP_PORT", "")
	t.Setenv("LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error %v, want nil", err)
	}
	if cfg.HTTPPort != "8080" {
		t.Errorf("cfg.HTTPPort = %q, want %q", cfg.HTTPPort, "8080")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("cfg.LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestLoadUsesExplicitOverridesOverDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/paygate?sslmode=disable")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "s3cret")
	t.Setenv("HTTP_PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error %v, want nil", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@localhost:5432/paygate?sslmode=disable" {
		t.Errorf("cfg.DatabaseURL = %q, want the configured DSN", cfg.DatabaseURL)
	}
	if cfg.WebhookSigningSecret != "s3cret" {
		t.Errorf("cfg.WebhookSigningSecret = %q, want %q", cfg.WebhookSigningSecret, "s3cret")
	}
	if cfg.HTTPPort != "9090" {
		t.Errorf("cfg.HTTPPort = %q, want %q", cfg.HTTPPort, "9090")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("cfg.LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
}
