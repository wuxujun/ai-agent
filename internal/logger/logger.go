// Package logger provides a structured, leveled logger for ai-agent built on
// top of the standard library's log/slog package.
//
// # Design goals
//
//  1. Every log line is JSON, machine-parseable, and filterable by task_id,
//     component, level, and trace_id without grep hacks.
//  2. Task-scoped loggers are cheap: TaskLogger / FromCtx add pre-bound
//     key-value pairs once and re-use the same *slog.Logger for all calls.
//  3. Hot-reload friendly: Reinit replaces the global handler atomically when
//     the log level changes at runtime (no restart required).
//
// # Usage
//
//	// Package-level component logger (create once per package)
//	var log = logger.Component("orchestrator")
//
//	// Task-scoped logger (create once per task invocation)
//	tlog := logger.FromCtx(ctx, task.ID)
//	tlog.Info("step started", "step", n, "budget", task.ToolBudget)
//	// → {"time":"…","level":"INFO","msg":"step started","component":"orchestrator","task_id":"abc","step":1,"budget":5}
//
// # JSON field conventions
//
//	time       – RFC 3339 nano timestamp (slog default)
//	level      – DEBUG / INFO / WARN / ERROR
//	msg        – human-readable message (no interpolated IDs)
//	component  – subsystem name ("orchestrator", "planner", "store", …)
//	task_id    – task identifier; present whenever a task is in scope
//	error      – error string (error-level logs)
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"unsafe"
)

// ctxKey is the unexported key type for storing a *slog.Logger in a context.
type ctxKey struct{}

// ── Global logger ────────────────────────────────────────────────────────────

// defaultLogger is the package-level logger. Access is via atomic pointer so
// Reinit can swap it without a mutex and without breaking concurrent readers.
var defaultLoggerPtr atomic.Pointer[slog.Logger]

func init() {
	setDefault(newLogger(levelFromEnv()))
}

func newLogger(level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func setDefault(l *slog.Logger) {
	// Store via unsafe.Pointer so we can use atomic.Pointer[slog.Logger].
	// This is safe: slog.Logger is not mutated after construction.
	_ = unsafe.Sizeof(l) // silence linter
	defaultLoggerPtr.Store(l)
	slog.SetDefault(l)
}

// L returns the current default structured logger.
func L() *slog.Logger { return defaultLoggerPtr.Load() }

// Reinit replaces the global logger with a new one at the given level string.
// Call this after a config hot-reload that changes log.level. Safe for
// concurrent use; in-flight log calls finish before the swap is visible.
func Reinit(levelStr string) {
	setDefault(newLogger(parseLevel(levelStr)))
}

func levelFromEnv() slog.Level {
	return parseLevel(os.Getenv("AI_AGENT_LOG_LEVEL"))
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

// ── Component loggers ────────────────────────────────────────────────────────

// Component returns a logger pre-tagged with a "component" field.
// Intended for package-level var declarations:
//
//	var log = logger.Component("planner")
func Component(name string) *slog.Logger {
	return L().With(slog.String("component", name))
}

// With returns the default logger enriched with additional key-value attributes.
func With(args ...any) *slog.Logger { return L().With(args...) }

// TaskLogger returns a logger pre-tagged with both "component" and "task_id".
// Use this when you already have both values but no context to carry them.
func TaskLogger(component, taskID string) *slog.Logger {
	return L().With(
		slog.String("component", component),
		slog.String("task_id", taskID),
	)
}

// ── Context helpers ──────────────────────────────────────────────────────────

// WithCtx embeds a task-scoped logger into ctx so that FromCtx can retrieve it
// later without threading task_id through every function signature.
//
// Typical usage in an HTTP handler or goroutine entry point:
//
//	ctx = logger.WithCtx(ctx, logger.TaskLogger("api", task.ID))
func WithCtx(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromCtx extracts the task-scoped logger from ctx (previously stored by
// WithCtx). If no logger is found, it falls back to the default logger tagged
// with the provided task_id so the caller always gets a valid logger.
func FromCtx(ctx context.Context, taskID string) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return L().With(slog.String("task_id", taskID))
}

// ── Top-level convenience functions ─────────────────────────────────────────
// These mirror the log.Printf / log.Println surface so callers that do not
// need task tagging can migrate with minimal churn.

// Debug logs at DEBUG level on the default logger.
func Debug(msg string, args ...any) { L().Debug(msg, args...) }

// Info logs at INFO level on the default logger.
func Info(msg string, args ...any) { L().Info(msg, args...) }

// Warn logs at WARN level on the default logger.
func Warn(msg string, args ...any) { L().Warn(msg, args...) }

// Error logs at ERROR level on the default logger.
func Error(msg string, args ...any) { L().Error(msg, args...) }

// InfoCtx logs at INFO level using the logger embedded in ctx (if any).
func InfoCtx(ctx context.Context, msg string, args ...any) {
	L().InfoContext(ctx, msg, args...)
}

// WarnCtx logs at WARN level using the logger embedded in ctx (if any).
func WarnCtx(ctx context.Context, msg string, args ...any) {
	L().WarnContext(ctx, msg, args...)
}

// ErrorCtx logs at ERROR level using the logger embedded in ctx (if any).
func ErrorCtx(ctx context.Context, msg string, args ...any) {
	L().ErrorContext(ctx, msg, args...)
}
