package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// This helper exercises the real import transaction with a payload much larger
// than the SQLite page cache. The parent kills it at an explicit SQL boundary.
func TestPersistentTaskRestoreCrashHelper(t *testing.T) {
	mode := os.Getenv("AI_AGENT_RESTORE_CRASH_MODE")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	var barrier func()
	if mode == "before_commit" {
		// Functions are installed on connections when they are opened.
		if err := sqlite.RegisterScalarFunction("restore_crash_barrier", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { barrier(); return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	sandbox, dir, err := CreatePersistentTaskRestoreSandbox(t.Context(), os.Getenv("AI_AGENT_RESTORE_CRASH_PARENT"))
	if err != nil {
		t.Fatal(err)
	}
	_, snap, _, err := decodeJointRestore(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	snap.Task.Goal = strings.Repeat("spill-fixture-", 80000)
	data, err := EncodeTaskExport(t.Context(), snap, "fixture", snap.Task.ID, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, taskRestoreDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	ready := func() {
		fmt.Printf("RESTORE_READY %s\n%x\n", dir, sha256.Sum256(before))
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		os.Exit(3)
	}
	barrier = ready
	if _, err := sandbox.db.Exec("PRAGMA cache_size=8; PRAGMA cache_spill=ON"); err != nil {
		t.Fatal(err)
	}
	if mode == "before_commit" {
		if _, err := sandbox.db.Exec("PRAGMA trusted_schema=ON; CREATE TEMP TRIGGER crash_barrier AFTER INSERT ON restore_parts WHEN NEW.kind='task' BEGIN SELECT restore_crash_barrier(); END"); err != nil {
			t.Fatal(err)
		}
	} else if mode != "after_commit" {
		t.Fatal("unknown crash mode")
	}
	if err := sandbox.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes); err != nil {
		t.Fatal(err)
	}
	ready()
}

func TestPersistentTaskRestoreForcedCrash(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, executable, "-test.run=^TestPersistentTaskRestoreCrashHelper$")
			command.Env = append(os.Environ(), "AI_AGENT_RESTORE_CRASH_MODE="+mode, "AI_AGENT_RESTORE_CRASH_PARENT="+t.TempDir())
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
			scanner := bufio.NewScanner(stdout)
			var dir string
			var diagnostics strings.Builder
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "RESTORE_READY ") {
					dir = strings.TrimPrefix(scanner.Text(), "RESTORE_READY ")
					break
				}
				diagnostics.WriteString(scanner.Text() + "\n")
			}
			if dir == "" {
				t.Fatalf("helper not ready: context=%v scan=%v\n%s%s", ctx.Err(), scanner.Err(), diagnostics.String(), stderr.String())
			}
			if !scanner.Scan() {
				t.Fatal("missing baseline hash")
			}
			baseline := scanner.Text()
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("helper was not forcibly terminated")
			}
			path := filepath.Join(dir, taskRestoreDatabaseName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) == baseline {
				t.Fatal("large transaction never spilled to database")
			}
			if mode == "before_commit" {
				journalPath := path + "-journal"
				journal, err := os.ReadFile(journalPath)
				// A real hot rollback journal has a valid synced header, rather than
				// merely an allocated/zeroed journal from a small cached transaction.
				if err != nil || len(journal) <= 512 || !bytes.Equal(journal[:8], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
					t.Fatal("no hot spill journal", err)
				}
				if s, err := OpenPersistentTaskRestoreSandbox(ctx, dir); !errors.Is(err, ErrTaskRestoreTarget) {
					if s != nil {
						s.Close()
					}
					t.Fatal("hot journal accepted", err)
				}
				afterJournal, err := os.ReadFile(journalPath)
				if err != nil || !bytes.Equal(journal, afterJournal) {
					t.Fatal("hot journal evidence changed", err)
				}
			} else {
				sandbox, err := OpenPersistentTaskRestoreSandbox(ctx, dir)
				if err != nil {
					t.Fatal(err)
				}
				report, err := sandbox.Verify(ctx, "restore-task", "a")
				sandbox.Close()
				if err != nil || !report.Verified || report.RestoreReady || !report.SourcePreserved {
					t.Fatal("committed crash verification failed", err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("read-only reopen changed crash database", err)
			}
		})
	}
}

func TestPersistentTaskRestoreFileDamage(t *testing.T) {
	for _, mode := range []string{"empty", "truncated", "header", "last_page"} {
		t.Run(mode, func(t *testing.T) {
			_, dir, err := VerifyPersistentTaskRestore(t.Context(), restoreFixture(t), "a", t.TempDir(), MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, taskRestoreDatabaseName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "empty":
				data = nil
			case "truncated":
				data = data[:len(data)/2]
			case "header":
				copy(data[:16], bytes.Repeat([]byte{0xff}, 16))
			case "last_page":
				copy(data[len(data)-4096:], bytes.Repeat([]byte{0xff}, 4096))
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir); !errors.Is(err, ErrTaskRestoreTarget) {
				if s != nil {
					s.Close()
				}
				t.Fatal("damaged file accepted", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, after) {
				t.Fatal("damaged evidence changed", err)
			}
		})
	}
}
