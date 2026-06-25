// Package logging provides structured logging built on zerolog.
//
// Output goes to stderr so that stdout stays clean for tool output and for
// capture inside a Nextflow task.
package logging

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New returns a logger configured from the LOG_LEVEL and LOG_FORMAT env vars.
// LOG_FORMAT=json emits JSON (suitable for capture inside a Nextflow task);
// any other value emits human-readable console output.
func New() zerolog.Logger {
	level, err := zerolog.ParseLevel(getenv("LOG_LEVEL", "info"))
	if err != nil {
		level = zerolog.InfoLevel
	}

	if getenv("LOG_FORMAT", "console") == "json" {
		return zerolog.New(os.Stderr).Level(level).With().Timestamp().Logger()
	}

	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.Kitchen}).
		Level(level).
		With().
		Timestamp().
		Logger()
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}
