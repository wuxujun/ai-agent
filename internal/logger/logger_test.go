package logger

import (
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/buildinfo"
)

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
