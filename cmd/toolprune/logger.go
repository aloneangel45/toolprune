package main

import (
	"log/slog"
	"os"
	"strings"
)

// setupLogger initializes slog writing strictly to os.Stderr to protect stdio JSON-RPC framing.
func setupLogger(levelStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "info":
		fallthrough
	default:
		level = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// bindEnvFallback sets a flag value from environment variable if not explicitly passed by user.
func bindEnvFallback(flagName, envVar string, setFunc func(string)) {
	if val := os.Getenv(envVar); val != "" {
		setFunc(val)
	}
}
