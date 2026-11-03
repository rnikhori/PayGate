package main

import (
	"fmt"
	"os"

	"github.com/rnikhori/paygate/internal/config"
)

// PayGate API entrypoint. Service wiring lands as the backlog in issues/ is
// implemented; today main only loads Config and fails fast if it's invalid,
// then passes it explicitly to whatever it wires up next (no global state).
func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}

	fmt.Printf("api: config loaded (port=%s log_level=%s)\n", cfg.HTTPPort, cfg.LogLevel)
}
