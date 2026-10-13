package store

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"modernc.org/sqlite"
)

func TestPersistentTaskRestoreReopenAndReadOnly(t *testing.T) {
	data := restoreFixture(t)
	report, dir, err := VerifyPersistentTaskRestore(t.Context(), data, "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || !report.Reopened || report.Target != "persistent_isolated_sqlite" || report.RestoreReady || !report.SourcePreserved || report.SourceBackend != "fixture" {
		t.Fatal("wrong durable report", report)
	}
	path := filepath.Join(dir, taskRestoreDatabaseName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, path} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm()&0077 != 0 {
				t.Fatal("sandbox not private", err)
			}
		}
	}
	reopened, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes); !errors.Is(err, ErrTaskRestoreReadOnly) {
		t.Fatal("reopened sandbox accepted writes", err)
	}
	if _, err := reopened.db.Exec("UPDATE restore_runs SET tenant_id='b'"); err == nil {
		t.Fatal("SQL read-only constraint absent")
	}
	if _, err := reopened.Verify(t.Context(), "restore-task", "b"); err == nil {
		t.Fatal("wrong tenant verified")
	}
	if _, err := reopened.Verify(t.Context(), "restore-task", "a"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("readback changed persisted file", err)
	}
}

func TestPersistentTaskRestoreRejectsForeignTargets(t *testing.T) {
	parent := t.TempDir()
	online, err := NewSQLiteStore(filepath.Join(parent, "online.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(parent, "online.db"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := os.MkdirTemp(parent, "foreign-")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(foreign)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(canonical, taskRestoreDatabaseName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPersistentTaskRestoreSandbox(t.Context(), canonical); !errors.Is(err, ErrTaskRestoreTarget) {
		t.Fatal("online database accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("foreign database changed", err)
	}
	if _, err := OpenPersistentTaskRestoreSandbox(t.Context(), filepath.Join(parent, "missing")); !errors.Is(err, ErrTaskRestoreTarget) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "missing")); !os.IsNotExist(err) {
		t.Fatal("open created target")
	}
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), restoreFixture(t), "a", parent, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(parent, "alias")
		if err := os.Symlink(dir, alias); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenPersistentTaskRestoreSandbox(t.Context(), alias); !errors.Is(err, ErrTaskRestoreTarget) {
			t.Fatal("symlink accepted", err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir); !errors.Is(err, ErrTaskRestoreTarget) {
			t.Fatal("public directory accepted", err)
		}
	}
}

func TestPersistentTaskRestoreCorruptionFailsClosed(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE restore_runs SET expected='null'",
		"UPDATE restore_runs SET payload_hash='bad'",
		"UPDATE restore_runs SET artifact='null'",
		"UPDATE restore_parts SET body='null' WHERE kind='task'",
		"DELETE FROM restore_parts WHERE kind='trace' AND position=0",
		"UPDATE restore_parts SET position=9 WHERE kind='memory'",
	} {
		t.Run(mutation, func(t *testing.T) {
			sandbox, dir, err := CreatePersistentTaskRestoreSandbox(t.Context(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := sandbox.ImportJoint(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes); err != nil {
				t.Fatal(err)
			}
			if _, err := sandbox.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			sandbox.Close()
			reopened, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Verify(t.Context(), "restore-task", "a"); !errors.Is(err, ErrTaskRestoreMismatch) {
				t.Fatal("corruption verified", err)
			}
		})
	}
}

func TestPersistentTaskRestoreCancelDuringImport(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	name := "restore_cancel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { cancel(); return int64(1), nil }); err != nil {
		t.Fatal(err)
	}
	sandbox, dir, err := CreatePersistentTaskRestoreSandbox(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// This fixture-only trigger cancels exactly after the first restored record.
	if _, err := sandbox.db.Exec("PRAGMA trusted_schema=ON; CREATE TEMP TRIGGER cancel_import AFTER INSERT ON restore_parts BEGIN SELECT " + name + "(); END"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ImportJoint(ctx, restoreFixture(t), "a", MaxTaskExportBytes); err == nil {
		t.Fatal("interrupted import succeeded")
	}
	sandbox.Close()
	reopened, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, table := range []string{"restore_runs", "restore_parts"} {
		var count int
		if err := reopened.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("interrupted import left records", table, count, err)
		}
	}
}

// Runs in a separate OS process. Exit bypasses Close and deferred rollback.
func TestPersistentTaskRestoreProcessHelper(t *testing.T) {
	mode := os.Getenv("AI_AGENT_RESTORE_PROCESS_MODE")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	sandbox, dir, err := CreatePersistentTaskRestoreSandbox(t.Context(), os.Getenv("AI_AGENT_RESTORE_PROCESS_PARENT"))
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "committed":
		if err := sandbox.ImportJoint(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes); err != nil {
			t.Fatal(err)
		}
	case "interrupted":
		tx, err := sandbox.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO restore_runs VALUES ('partial','a','{}','bad','{}')"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO restore_parts VALUES ('partial','task',0,'{}')"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown helper mode")
	}
	fmt.Print(dir)
	os.Exit(0)
}

func TestPersistentTaskRestoreAfterProcessExit(t *testing.T) {
	for _, mode := range []string{"committed", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestPersistentTaskRestoreProcessHelper$")
			command.Env = append(os.Environ(), "AI_AGENT_RESTORE_PROCESS_MODE="+mode, "AI_AGENT_RESTORE_PROCESS_PARENT="+t.TempDir())
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("helper failed: %v %s", err, output)
			}
			dir := strings.TrimSpace(string(output))
			sandbox, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Close()
			if mode == "committed" {
				report, err := sandbox.Verify(t.Context(), "restore-task", "a")
				if err != nil || !report.Reopened || !report.Verified {
					t.Fatal("committed process data missing", err)
				}
			} else {
				for _, table := range []string{"restore_runs", "restore_parts"} {
					var count int
					if err := sandbox.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
						t.Fatal("process interruption exposed partial records", err)
					}
				}
			}
		})
	}
}

func TestPersistentTaskRestoreInvalidInputCreatesNoDirectory(t *testing.T) {
	parent := t.TempDir()
	if _, dir, err := VerifyPersistentTaskRestore(t.Context(), []byte(`null`), "a", parent, MaxTaskExportBytes); err == nil || dir != "" {
		t.Fatal("invalid input allocated target")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid input left files", err)
	}
}

func TestPersistentTaskRestoreRejectsSchemaChanges(t *testing.T) {
	for _, mutation := range []string{"PRAGMA application_id=0", "PRAGMA user_version=2", "CREATE TABLE sqliteXunexpected (id TEXT)", "ALTER TABLE restore_runs RENAME TO other_runs"} {
		t.Run(mutation, func(t *testing.T) {
			sandbox, dir, err := CreatePersistentTaskRestoreSandbox(t.Context(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sandbox.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			sandbox.Close()
			path := filepath.Join(dir, taskRestoreDatabaseName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir); !errors.Is(err, ErrTaskRestoreTarget) {
				t.Fatal("altered database accepted", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected open mutated database", err)
			}
		})
	}
}
