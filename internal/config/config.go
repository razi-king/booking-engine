// Package config reads settings from environment variables (12-factor style):
// the same binary runs on a laptop, in Docker and in production; only the
// environment changes.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port           string
	DatabaseURL    string        // empty → in-memory store
	HoldTTL        time.Duration // how long a seat hold lasts
	MaxSeats       int           // seats per hold
	SweepFallback  time.Duration // scheduler sweeps at least this often
	IdemRetention  time.Duration // how long idempotency keys are kept
	AllowedOrigins []string
}

// Load reads the environment, using defaults for anything unset.
func Load() Config {
	return Config{
		Port:           str("PORT", "8080"),
		DatabaseURL:    str("DATABASE_URL", ""),
		HoldTTL:        dur("HOLD_TTL", 10*time.Minute),
		MaxSeats:       num("MAX_SEATS", 9),
		SweepFallback:  dur("SWEEP_EVERY", time.Minute),
		IdemRetention:  dur("IDEMPOTENCY_RETENTION", 24*time.Hour),
		AllowedOrigins: strings.Split(str("ALLOWED_ORIGINS", "*"), ","),
	}
}

// The three helpers share one shape: read, fall back to the default if empty
// or unparsable. Small and explicit beats a reflection-based config library here.
func str(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func num(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
