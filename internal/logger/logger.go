// Package logger provides a structured, leveled logger for ai-agent.
// It wraps the standard library slog package with a consistent format
// and exposes helper functions that match the existing log.Printf call sites.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

var defaultLogger *slog.Logger

func init() {
	level := parseLevel(os.Getenv("AI_AGENT_LOG_LEVEL"))
	opts := &slog.HandlerOptions{Level: level}
	handler := slog.NewJSONHandler(os.Stdout, opts)
	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
}

func parseLevel(s string) slog.Level {
	switch strings.ToUpper(s) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// L returns the default structured logger.
func L() *slog.Logger { return defaultLogger }

// With returns a logger enriched with additional key-value attributes.
func With(args ...any) *slog.Logger { return defaultLogger.With(args...) }

// TaskLogger returns a logger pre-tagged with task_id.
func TaskLogger(taskID string) *slog.Logger {
	return defaultLogger.With(slog.String("task_id", taskID))
}

// Debug logs at DEBUG level.
func Debug(msg string, args ...any) { defaultLogger.Debug(msg, args...) }

// Info logs at INFO level.
func Info(msg string, args ...any) { defaultLogger.Info(msg, args...) }

// Warn logs at WARN level.
func Warn(msg string, args ...any) { defaultLogger.Warn(msg, args...) }

// Error logs at ERROR level.
func Error(msg string, args ...any) { defaultLogger.Error(msg, args...) }

// InfoCtx logs at INFO level with a context (for trace propagation).
func InfoCtx(ctx context.Context, msg string, args ...any) {
	defaultLogger.InfoContext(ctx, msg, args...)
}

// ErrorCtx logs at ERROR level with a context.
func ErrorCtx(ctx context.Context, msg string, args ...any) {
	defaultLogger.ErrorContext(ctx, msg, args...)
}
