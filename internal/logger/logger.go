// Package logger provides structured JSON logging to the console, to
// level-specific daily files, and to isolated task-report and access files.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuxujun/ai-agent/internal/buildinfo"
)

type ctxKey struct{}

// Options controls console, level-specific file, and access logging.
// RetentionDays is based on the date in the rotated filename; zero keeps files
// indefinitely.
type Options struct {
	Level         string
	Console       bool
	FileEnabled   bool
	AccessEnabled bool
	Directory     string
	RetentionDays int
}

type handlerState struct {
	mu            sync.RWMutex
	handler       slog.Handler
	reportHandler slog.Handler
	accessHandler slog.Handler
	closer        io.Closer
}

// dynamicHandler keeps package-level component loggers hot-reloadable. A
// derived slog.Logger retains its attributes, while every record uses the
// latest configured output handler.
type dynamicHandler struct {
	state      *atomic.Pointer[handlerState]
	report     bool
	access     bool
	operations []handlerOperation
}

type handlerOperation struct {
	group string
	attrs []slog.Attr
}

func newJSONHandler(writer io.Writer, options *slog.HandlerOptions) slog.Handler {
	return slog.NewJSONHandler(writer, options).WithAttrs([]slog.Attr{
		slog.String("app_version", buildinfo.Current()),
	})
}

func (h *dynamicHandler) current() *handlerState { return h.state.Load() }

func (h *dynamicHandler) resolved(s *handlerState) slog.Handler {
	target := s.handler
	if h.report {
		target = s.reportHandler
	} else if h.access {
		target = s.accessHandler
	}
	for _, operation := range h.operations {
		if operation.group != "" {
			target = target.WithGroup(operation.group)
		} else {
			target = target.WithAttrs(operation.attrs)
		}
	}
	return target
}

func (h *dynamicHandler) Enabled(ctx context.Context, level slog.Level) bool {
	s := h.current()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return h.resolved(s).Enabled(ctx, level)
}

func (h *dynamicHandler) Handle(ctx context.Context, record slog.Record) error {
	s := h.current()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return h.resolved(s).Handle(ctx, record)
}

func (h *dynamicHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := *h
	cloned.operations = append(append([]handlerOperation(nil), h.operations...), handlerOperation{attrs: append([]slog.Attr(nil), attrs...)})
	return &cloned
}

func (h *dynamicHandler) WithGroup(name string) slog.Handler {
	cloned := *h
	cloned.operations = append(append([]handlerOperation(nil), h.operations...), handlerOperation{group: name})
	return &cloned
}

type multiHandler []slog.Handler

func (h multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, item := range h {
		if item.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h multiHandler) Handle(ctx context.Context, record slog.Record) error {
	var firstErr error
	for _, item := range h {
		if item.Enabled(ctx, record.Level) {
			if err := item.Handle(ctx, record); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (h multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	result := make(multiHandler, len(h))
	for i, item := range h {
		result[i] = item.WithAttrs(attrs)
	}
	return result
}

func (h multiHandler) WithGroup(name string) slog.Handler {
	result := make(multiHandler, len(h))
	for i, item := range h {
		result[i] = item.WithGroup(name)
	}
	return result
}

type exactLevelHandler struct {
	level   slog.Level
	minimum slog.Level
	handler slog.Handler
}

func (h exactLevelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minimum && normalizedLevel(level) == h.level
}
func (h exactLevelHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.handler.Handle(ctx, record)
}
func (h exactLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return exactLevelHandler{level: h.level, minimum: h.minimum, handler: h.handler.WithAttrs(attrs)}
}
func (h exactLevelHandler) WithGroup(name string) slog.Handler {
	return exactLevelHandler{level: h.level, minimum: h.minimum, handler: h.handler.WithGroup(name)}
}

func normalizedLevel(level slog.Level) slog.Level {
	switch {
	case level < slog.LevelInfo:
		return slog.LevelDebug
	case level < slog.LevelWarn:
		return slog.LevelInfo
	case level < slog.LevelError:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

type dailyWriter struct {
	mu            sync.Mutex
	directory     string
	levelName     string
	retentionDays int
	date          string
	file          *os.File
	now           func() time.Time
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	date := w.now().Format(time.DateOnly)
	if w.file == nil || w.date != date {
		if err := w.rotate(date); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

func (w *dailyWriter) rotate(date string) error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if err := os.MkdirAll(w.directory, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(w.directory, w.levelName+"-"+date+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	w.file = file
	w.date = date
	if err := w.removeExpired(); err != nil {
		_ = file.Close()
		w.file = nil
		return fmt.Errorf("remove expired %s logs: %w", w.levelName, err)
	}
	return nil
}

func (w *dailyWriter) removeExpired() error {
	if w.retentionDays <= 0 {
		return nil
	}
	entries, err := os.ReadDir(w.directory)
	if err != nil {
		return err
	}
	cutoff := w.now().AddDate(0, 0, -w.retentionDays)
	prefix := w.levelName + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		dateText := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".log")
		date, parseErr := time.ParseInLocation(time.DateOnly, dateText, w.now().Location())
		if parseErr == nil && date.Before(time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())) {
			if err := os.Remove(filepath.Join(w.directory, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

type closeGroup []io.Closer

func (g closeGroup) Close() error {
	var firstErr error
	for _, closer := range g {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var state atomic.Pointer[handlerState]
var defaultLogger *slog.Logger
var reportLogger *slog.Logger
var accessLogger *slog.Logger

func init() {
	initial := &handlerState{
		handler:       newJSONHandler(os.Stdout, &slog.HandlerOptions{Level: levelFromEnv()}),
		reportHandler: newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
		accessHandler: newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	state.Store(initial)
	defaultLogger = slog.New(&dynamicHandler{state: &state})
	reportLogger = slog.New(&dynamicHandler{state: &state, report: true})
	accessLogger = slog.New(&dynamicHandler{state: &state, access: true})
	slog.SetDefault(defaultLogger)
}

// Configure atomically applies logging outputs. Existing component loggers
// immediately pick up the new settings.
func Configure(options Options) error {
	if options.RetentionDays < 0 {
		return fmt.Errorf("retention days must be greater than or equal to zero")
	}
	level := parseLevel(options.Level)
	handlerOptions := &slog.HandlerOptions{Level: level}
	var handlers multiHandler
	var closers closeGroup
	reportHandler := newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	accessHandler := newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	if options.Console {
		handlers = append(handlers, newJSONHandler(os.Stdout, handlerOptions))
	}
	if options.FileEnabled || options.AccessEnabled {
		directory := strings.TrimSpace(options.Directory)
		if directory == "" {
			return fmt.Errorf("log directory must not be empty when file or access logging is enabled")
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
		if options.FileEnabled {
			for _, spec := range []struct {
				name  string
				level slog.Level
			}{{"debug", slog.LevelDebug}, {"info", slog.LevelInfo}, {"warn", slog.LevelWarn}, {"error", slog.LevelError}} {
				writer := &dailyWriter{directory: directory, levelName: spec.name, retentionDays: options.RetentionDays, now: time.Now}
				// Rotate now so configuration failures are reported during startup,
				// rather than being silently delayed until the first record.
				writer.mu.Lock()
				err := writer.rotate(time.Now().Format(time.DateOnly))
				writer.mu.Unlock()
				if err != nil {
					_ = closeGroup(closers).Close()
					return err
				}
				closers = append(closers, writer)
				jsonHandler := newJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelDebug})
				handlers = append(handlers, exactLevelHandler{level: spec.level, minimum: level, handler: jsonHandler})
			}

			reportWriter := &dailyWriter{directory: directory, levelName: "task-report", retentionDays: options.RetentionDays, now: time.Now}
			reportWriter.mu.Lock()
			err := reportWriter.rotate(time.Now().Format(time.DateOnly))
			reportWriter.mu.Unlock()
			if err != nil {
				_ = closeGroup(closers).Close()
				return err
			}
			closers = append(closers, reportWriter)
			reportHandler = newJSONHandler(reportWriter, &slog.HandlerOptions{Level: slog.LevelDebug})
		}

		if options.AccessEnabled {
			accessWriter := &dailyWriter{directory: directory, levelName: "access", retentionDays: options.RetentionDays, now: time.Now}
			accessWriter.mu.Lock()
			err := accessWriter.rotate(time.Now().Format(time.DateOnly))
			accessWriter.mu.Unlock()
			if err != nil {
				_ = closeGroup(closers).Close()
				return err
			}
			closers = append(closers, accessWriter)
			accessHandler = newJSONHandler(accessWriter, &slog.HandlerOptions{Level: slog.LevelDebug})
		}
	}
	if len(handlers) == 0 {
		return fmt.Errorf("at least one log output must be enabled")
	}
	newState := &handlerState{handler: handlers, reportHandler: reportHandler, accessHandler: accessHandler, closer: closers}
	old := state.Swap(newState)
	if old != nil {
		old.mu.Lock()
		if old.closer != nil {
			_ = old.closer.Close()
		}
		old.mu.Unlock()
	}
	return nil
}

// Reinit preserves the existing API for callers that only change log level.
func Reinit(level string) {
	newState := &handlerState{
		handler:       newJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)}),
		reportHandler: newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
		accessHandler: newJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	old := state.Swap(newState)
	if old != nil {
		old.mu.Lock()
		if old.closer != nil {
			_ = old.closer.Close()
		}
		old.mu.Unlock()
	}
}

// Close flushes and closes active log files.
func Close() error {
	current := state.Load()
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.closer == nil {
		return nil
	}
	err := current.closer.Close()
	current.closer = nil
	return err
}

func levelFromEnv() slog.Level { return parseLevel(os.Getenv("AI_AGENT_LOG_LEVEL")) }

func parseLevel(value string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(value)) {
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

func L() *slog.Logger                    { return defaultLogger }
func Component(name string) *slog.Logger { return L().With(slog.String("component", name)) }

// ReportComponent returns a logger isolated from console and level-specific
// outputs. When file logging is enabled, records are written only to the
// daily task-report file.
func ReportComponent(name string) *slog.Logger {
	return reportLogger.With(slog.String("component", name))
}

// AccessComponent returns a logger isolated from console, level-specific, and
// task-report outputs. When access logging is enabled, records are written only
// to the daily access file.
func AccessComponent(name string) *slog.Logger {
	return accessLogger.With(slog.String("component", name))
}
func With(args ...any) *slog.Logger { return L().With(args...) }
func TaskLogger(component, taskID string) *slog.Logger {
	return L().With(slog.String("component", component), slog.String("task_id", taskID))
}
func WithCtx(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}
func FromCtx(ctx context.Context, taskID string) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return L().With(slog.String("task_id", taskID))
}
func Debug(msg string, args ...any)                         { L().Debug(msg, args...) }
func Info(msg string, args ...any)                          { L().Info(msg, args...) }
func Warn(msg string, args ...any)                          { L().Warn(msg, args...) }
func Error(msg string, args ...any)                         { L().Error(msg, args...) }
func InfoCtx(ctx context.Context, msg string, args ...any)  { L().InfoContext(ctx, msg, args...) }
func WarnCtx(ctx context.Context, msg string, args ...any)  { L().WarnContext(ctx, msg, args...) }
func ErrorCtx(ctx context.Context, msg string, args ...any) { L().ErrorContext(ctx, msg, args...) }
