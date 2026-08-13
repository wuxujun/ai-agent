package logger

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/buildinfo"
)

func TestDynamicHandlerRedactsSensitiveContentAndCredentials(t *testing.T) {
	var output bytes.Buffer
	state := &handlerState{handler: slog.NewJSONHandler(&output, nil)}
	var pointer atomic.Pointer[handlerState]
	pointer.Store(state)
	handler := &dynamicHandler{state: &pointer}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "safe message", 0)
	record.AddAttrs(
		slog.String("goal", "private customer question"),
		slog.String("redis_url", "redis://user:password@example:6379/0"),
		slog.String("service_token", "another-secret"),
		slog.Int("total_tokens", 42),
		slog.Group("nested", slog.String("query", "private search"), slog.String("status", "ok")),
	)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	derived := handler.WithAttrs([]slog.Attr{slog.String("goal", "derived private goal")})
	derivedRecord := slog.NewRecord(time.Now(), slog.LevelInfo, "derived message", 0)
	if err := derived.Handle(context.Background(), derivedRecord); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	for _, forbidden := range []string{"private customer question", "private search", "user:password", "another-secret", "derived private goal"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("sensitive value leaked into log: %s", logged)
		}
	}
	for _, expected := range []string{`"goal":"<25 chars>"`, `"redis_url":"[REDACTED]"`, `"service_token":"[REDACTED]"`, `"total_tokens":42`, `"query":"<14 chars>"`, `"status":"ok"`, `"goal":"<20 chars>"`} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("safe log metadata %q missing: %s", expected, logged)
		}
	}
}

func TestContentAndCredentialFieldPolicy(t *testing.T) {
	contentKeys := []string{"arguments", "body", "command", "content", "endpoint", "final_answer", "goal", "input", "instruction", "observation", "output", "params", "payload", "prompt", "query", "request", "response", "thought", "uri", "url", "user_input"}
	for _, key := range contentKeys {
		attr := safeLogAttr(slog.Any(key, map[string]any{"private": "customer data"}))
		if got := attr.Value.String(); !strings.HasPrefix(got, "<") || !strings.HasSuffix(got, " chars>") {
			t.Errorf("content field %q was not summarized: %q", key, got)
		}
	}
	secretKeys := []string{"api_key", "authorization", "cookie", "credential", "database_dsn", "password", "private_key", "redis_url", "service_secret", "set_cookie", "session_token"}
	for _, key := range secretKeys {
		if got := safeLogAttr(slog.String(key, "secret-value")).Value.String(); got != "[REDACTED]" {
			t.Errorf("credential field %q = %q", key, got)
		}
	}
}

func TestTaskIDContext(t *testing.T) {
	ctx := WithTaskID(context.Background(), " task-123 ")
	if got := TaskID(ctx); got != "task-123" {
		t.Fatalf("TaskID() = %q, want task-123", got)
	}
	if got := TaskID(context.Background()); got != "" {
		t.Fatalf("TaskID(background) = %q, want empty", got)
	}
	if got := TaskID(WithTaskID(ctx, "")); got != "task-123" {
		t.Fatalf("empty task ID replaced existing context value: %q", got)
	}
}

// TestProductionLogMessagesAreStatic prevents user-controlled values from
// bypassing attribute redaction by being formatted into the log message.
func TestProductionLogMessagesAreStatic(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	files := token.NewFileSet()
	for _, root := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repositoryRoot, root), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(files, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !isLogLevelMethod(selector.Sel.Name) {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok || !isLoggerIdentifier(receiver.Name) {
					return true
				}
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					position := files.Position(call.Args[0].Pos())
					t.Errorf("dynamic log message at %s:%d; use a static message and structured attributes", position.Filename, position.Line)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func isLogLevelMethod(name string) bool {
	return name == "Debug" || name == "Info" || name == "Warn" || name == "Error"
}

func isLoggerIdentifier(name string) bool {
	lower := strings.ToLower(name)
	return lower == "logger" || lower == "slog" || lower == "log" || strings.HasSuffix(lower, "log")
}

func TestConfigureRoutesRecordsToExactLevelFiles(t *testing.T) {
	directory := t.TempDir()
	if err := Configure(Options{
		Level:         "debug",
		FileEnabled:   true,
		Directory:     directory,
		RetentionDays: 7,
	}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
		Reinit("info")
	})

	component := Component("routing-test")
	component.Debug("debug record")
	component.Info("info record")
	component.Warn("warn record")
	component.Error("error record")

	date := time.Now().Format(time.DateOnly)
	tests := []struct {
		level string
		want  string
	}{
		{"debug", "debug record"},
		{"info", "info record"},
		{"warn", "warn record"},
		{"error", "error record"},
	}
	for _, tt := range tests {
		content, err := os.ReadFile(filepath.Join(directory, tt.level+"-"+date+".log"))
		if err != nil {
			t.Fatalf("read %s log: %v", tt.level, err)
		}
		text := string(content)
		if !strings.Contains(text, tt.want) || !strings.Contains(text, `"component":"routing-test"`) || !strings.Contains(text, `"app_version":"`+buildinfo.Current()+`"`) {
			t.Errorf("%s log missing its record or component: %s", tt.level, text)
		}
		for _, other := range tests {
			if other.level != tt.level && strings.Contains(text, other.want) {
				t.Errorf("%s record was also written to %s log", other.level, tt.level)
			}
		}
	}
}

func TestConfigureHonorsMinimumLevel(t *testing.T) {
	directory := t.TempDir()
	if err := Configure(Options{Level: "warn", FileEnabled: true, Directory: directory}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
		Reinit("info")
	})

	Debug("filtered debug")
	Info("filtered info")
	Warn("visible warn")

	date := time.Now().Format(time.DateOnly)
	debugContent, err := os.ReadFile(filepath.Join(directory, "debug-"+date+".log"))
	if err != nil {
		t.Fatal(err)
	}
	infoContent, err := os.ReadFile(filepath.Join(directory, "info-"+date+".log"))
	if err != nil {
		t.Fatal(err)
	}
	warnContent, err := os.ReadFile(filepath.Join(directory, "warn-"+date+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(debugContent) != 0 || len(infoContent) != 0 {
		t.Fatalf("records below configured level were written: debug=%q info=%q", debugContent, infoContent)
	}
	if !strings.Contains(string(warnContent), "visible warn") {
		t.Fatalf("warn record missing: %s", warnContent)
	}
}

func TestReportComponentWritesOnlyTaskReportFile(t *testing.T) {
	directory := t.TempDir()
	if err := Configure(Options{Level: "debug", FileEnabled: true, Directory: directory}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
		Reinit("info")
	})

	Component("normal").Info("normal record")
	stdlog.Print("standard library record")
	ReportComponent("api").Info("task report record", "task_id", "task-1")

	date := time.Now().Format(time.DateOnly)
	reportContent, err := os.ReadFile(filepath.Join(directory, "task-report-"+date+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if reportText := string(reportContent); !strings.Contains(reportText, "task report record") || !strings.Contains(reportText, `"component":"api"`) || !strings.Contains(reportText, `"app_version":"`+buildinfo.Current()+`"`) {
		t.Fatalf("task report file missing report record: %s", reportText)
	}

	for _, level := range []string{"debug", "info", "warn", "error"} {
		content, err := os.ReadFile(filepath.Join(directory, level+"-"+date+".log"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "task report record") {
			t.Fatalf("task report leaked into %s log: %s", level, content)
		}
		if level == "info" && (!strings.Contains(string(content), "standard library record") || !strings.Contains(string(content), `"app_version":"`+buildinfo.Current()+`"`)) {
			t.Fatalf("standard library log was not bridged with app version: %s", content)
		}
	}
	if strings.Contains(string(reportContent), "normal record") {
		t.Fatalf("normal log leaked into task report: %s", reportContent)
	}
}

func TestAccessComponentWritesOnlyAccessFile(t *testing.T) {
	directory := t.TempDir()
	if err := Configure(Options{Level: "debug", FileEnabled: true, AccessEnabled: true, Directory: directory}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
		Reinit("info")
	})

	Component("normal").Info("normal record")
	ReportComponent("api").Info("task report record")
	AccessComponent("access").Info("http request", "method", "GET", "status", 200)

	date := time.Now().Format(time.DateOnly)
	accessContent, err := os.ReadFile(filepath.Join(directory, "access-"+date+".log"))
	if err != nil {
		t.Fatal(err)
	}
	accessText := string(accessContent)
	if !strings.Contains(accessText, "http request") || !strings.Contains(accessText, `"component":"access"`) || !strings.Contains(accessText, `"app_version":"`+buildinfo.Current()+`"`) {
		t.Fatalf("access file missing request record: %s", accessText)
	}
	if strings.Contains(accessText, "normal record") || strings.Contains(accessText, "task report record") {
		t.Fatalf("non-access record leaked into access file: %s", accessText)
	}
	for _, filename := range []string{"info-" + date + ".log", "task-report-" + date + ".log"} {
		content, readErr := os.ReadFile(filepath.Join(directory, filename))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), "http request") {
			t.Fatalf("access record leaked into %s: %s", filename, content)
		}
	}
}

func TestDailyWriterRotatesAndRemovesExpiredFiles(t *testing.T) {
	directory := t.TempDir()
	current := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.Local)
	writer := &dailyWriter{
		directory:     directory,
		levelName:     "info",
		retentionDays: 2,
		now:           func() time.Time { return current },
	}
	oldPath := filepath.Join(directory, "info-2026-07-10.log")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("day one\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired file was not removed, stat error = %v", err)
	}

	current = current.AddDate(0, 0, 1)
	if _, err := writer.Write([]byte("day two\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"info-2026-07-15.log", "info-2026-07-16.log"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Errorf("rotated file %s missing: %v", name, err)
		}
	}
}
