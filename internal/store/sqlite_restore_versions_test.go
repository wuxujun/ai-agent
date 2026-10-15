package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func restoreVersionStore(t *testing.T) *SQLiteStore {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := NewSQLiteStoreWithRestoreVersions(filepath.Join(directory, "rows.sqlite"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
func restoreVersionRead(t *testing.T, st *SQLiteStore) *SQLiteRestoreVersionObservation {
	t.Helper()
	out, err := st.ReadSQLiteRestoreVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func restoreVersionTask(t *testing.T, st *SQLiteStore) *types.Task {
	t.Helper()
	task := &types.Task{ID: "task", TenantID: "tenant", Goal: "secret-goal", Status: types.StatusPaused}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	return task
}
func restoreVersionFind(t *testing.T, out *SQLiteRestoreVersionObservation, kind, id string) SQLiteRestoreRowVersion {
	t.Helper()
	for _, v := range out.Versions {
		if v.Kind == kind && v.ID == id {
			return v
		}
	}
	t.Fatalf("missing %s", kind)
	return SQLiteRestoreRowVersion{}
}
func restoreVersionPin(out *SQLiteRestoreVersionObservation) SQLiteRestoreVersionPin {
	return SQLiteRestoreVersionPin{Origin: out.Origin, NamespaceSHA256: out.NamespaceSHA256, SchemaSHA256: out.SchemaSHA256, MinimumEpoch: out.Epoch, PrefixEventsSHA256: out.EventsSHA256}
}

func TestSQLiteRestoreVersionsCoreWriterCoverage(t *testing.T) {
	st := restoreVersionStore(t)
	zero := restoreVersionRead(t, st)
	if zero.Epoch != 0 || zero.Status != "unverified" || len(zero.Gaps) != 8 {
		t.Fatal("baseline")
	}
	session := &types.Session{ID: "session", TenantID: "tenant", Title: "secret-title"}
	if err := st.CreateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NextSessionTaskSequence(t.Context(), session.ID, session.TenantID); err != nil {
		t.Fatal(err)
	}
	session.Title = "changed"
	if err := st.UpdateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	task := restoreVersionTask(t, st)
	task.SessionID = session.ID
	task.SequenceNo = 1
	if err := st.SaveTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceTraces(t.Context(), task.ID, []types.StepTrace{{Step: 1, Observation: "secret-trace"}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "secret-owner", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before := restoreVersionRead(t, st)
	if ok, err := st.AcquireTaskLease(t.Context(), task.ID, "other-owner", time.Hour); err != nil || ok {
		t.Fatal(ok, err)
	}
	if after := restoreVersionRead(t, st); !reflect.DeepEqual(before, after) {
		t.Fatal("failed acquire changed versions")
	}
	if ok, err := st.RenewTaskLease(t.Context(), task.ID, "secret-owner", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before = restoreVersionRead(t, st)
	ctx := WithTaskLease(t.Context(), task.ID, "secret-owner")
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	after := restoreVersionRead(t, st)
	// Lease guard's UPDATE owner=owner must not manufacture a lease version.
	if restoreVersionFind(t, before, "lease", task.ID) != restoreVersionFind(t, after, "lease", task.ID) {
		t.Fatal("lease guard changed version")
	}
	if ok, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusPaused}, types.StatusRunning); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := st.ReleaseTaskLease(t.Context(), task.ID, "secret-owner"); err != nil {
		t.Fatal(err)
	}
	out := restoreVersionRead(t, st)
	for _, kind := range []string{"task", "session", "trace", "lease"} {
		found := false
		for _, e := range out.Events {
			found = found || e.Kind == kind
		}
		if !found {
			t.Fatal("writer omitted", kind)
		}
	}
	if !restoreVersionFind(t, out, "lease", task.ID).Deleted {
		t.Fatal("lease tombstone")
	}
	report, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-goal", "secret-title", "secret-trace", "secret-owner"} {
		if bytes.Contains(report, []byte(secret)) {
			t.Fatal("diagnostic leaked body")
		}
	}
	body := sqliteRestoreVersionHash(out)
	if len(body) != 64 {
		t.Fatal("digest")
	}
	if ok, err := st.DeleteTask(t.Context(), task.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	deleted := restoreVersionRead(t, st)
	if !restoreVersionFind(t, deleted, "task", task.ID).Deleted {
		t.Fatal("task tombstone")
	}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	recreated := restoreVersionRead(t, st)
	if restoreVersionFind(t, recreated, "task", task.ID).Incarnation <= restoreVersionFind(t, out, "task", task.ID).Incarnation {
		t.Fatal("ABA incarnation reused")
	}
	if _, err := st.DeleteAllTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	final := restoreVersionRead(t, st)
	if !restoreVersionFind(t, final, "task", task.ID).Deleted {
		t.Fatal("delete-all not covered")
	}
}

func TestSQLiteRestoreVersionsAtomicRollbackAndDirectABA(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	before := restoreVersionRead(t, st)
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET goal='different' WHERE id='task'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET goal='secret-goal' WHERE id='task'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after := restoreVersionRead(t, st)
	if after.DataSHA256 != before.DataSHA256 || after.Epoch != before.Epoch+2 || after.EventsSHA256 == before.EventsSHA256 {
		t.Fatal("direct SQL ABA invisible")
	}
	tx, err = st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET goal='rollback'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if out := restoreVersionRead(t, st); !reflect.DeepEqual(out, after) {
		t.Fatal("rollback retained row/version")
	}
	// A failed recorder must roll back the business DML, not just its journal.
	if _, err := st.db.Exec(`UPDATE restore_row_meta SET epoch=8192`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE tasks SET goal='overflow'`); err == nil {
		t.Fatal("overflow accepted")
	}
	task, err := st.GetTask(t.Context(), "task")
	if err != nil || task.Goal != "secret-goal" {
		t.Fatal("business changed despite recorder failure", err)
	}
	if _, err := st.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("tampered head accepted", err)
	}
}

func TestSQLiteRestoreVersionsFailClosed(t *testing.T) {
	for _, query := range []string{
		`DROP TRIGGER restore_row_tasks_update`,
		`DELETE FROM restore_row_events`,
		`UPDATE restore_row_versions SET incarnation=epoch+1`,
		`UPDATE restore_row_meta SET origin='wrong'`,
		`CREATE TABLE unknown_domain(id TEXT)`,
	} {
		t.Run(query, func(t *testing.T) {
			st := restoreVersionStore(t)
			restoreVersionTask(t, st)
			if _, err := st.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
				t.Fatal("accepted", err)
			}
		})
	}
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	before := restoreVersionRead(t, st)
	if _, err := st.db.Exec(`UPDATE tasks SET id='changed'`); err == nil {
		t.Fatal("identity rewrite accepted")
	}
	if out := restoreVersionRead(t, st); !reflect.DeepEqual(out, before) {
		t.Fatal("identity failure changed observation")
	}
	if _, err := st.db.Exec(`UPDATE tasks SET goal=?`, strings.Repeat("x", sqliteRestoreVersionMaxBytes+1)); err == nil {
		t.Fatal("oversized business write accepted")
	}
	if out := restoreVersionRead(t, st); !reflect.DeepEqual(out, before) {
		t.Fatal("oversized write changed data/version")
	}

}

func TestSQLiteRestoreVersionsReadOnlyReopenAndPin(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	before := restoreVersionRead(t, st)
	path := st.restoreVersions.path
	if _, err := st.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	var changes int64
	if err := st.db.QueryRow("SELECT total_changes()").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	bytesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if out := restoreVersionRead(t, st); !reflect.DeepEqual(out, before) {
			t.Fatal("unstable read")
		}
	}
	var changesAfter int64
	if err := st.db.QueryRow("SELECT total_changes()").Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	bytesAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if changes != changesAfter || !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("read modified database")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenSQLiteRestoreVersionReader(path, restoreVersionPin(before))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	out, err := reader.ReadSQLiteRestoreVersions(t.Context())
	if err != nil || !reflect.DeepEqual(out, before) {
		t.Fatal("reopen mismatch", err)
	}
	if _, err := reader.store.db.Exec(`UPDATE tasks SET goal='write'`); err == nil {
		t.Fatal("reader writable")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("read created sidecar")
		}
	}
	pin := restoreVersionPin(before)
	pin.MinimumEpoch++
	if r, err := OpenSQLiteRestoreVersionReader(path, pin); err == nil {
		r.Close()
		t.Fatal("rollback accepted")
	}
	pin = restoreVersionPin(before)
	pin.PrefixEventsSHA256 = strings.Repeat("0", 64)
	if r, err := OpenSQLiteRestoreVersionReader(path, pin); err == nil {
		r.Close()
		t.Fatal("fork pin accepted")
	}
	bytesAfter, _ = os.ReadFile(path)
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("reopen changed bytes")
	}
}

func TestSQLiteRestoreVersionsOptInAndCancellation(t *testing.T) {
	plain, err := NewSQLiteStore(filepath.Join(t.TempDir(), "plain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsUnavailable) {
		t.Fatal(err)
	}
	var count int
	if err := plain.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'restore_row_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("default migration", count, err)
	}
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	if s, err := NewSQLiteStoreWithRestoreVersions(st.restoreVersions.path, strings.Repeat("a", 64)); err == nil {
		s.Close()
		t.Fatal("adopted existing source")
	}
	if s, err := NewSQLiteStoreWithRestoreVersions(":memory:", strings.Repeat("a", 64)); err == nil {
		s.Close()
		t.Fatal("DSN accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.ReadSQLiteRestoreVersions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := st.ReadSQLiteRestoreVersions(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock wait ignored cancellation", err)
	}
	tx.Rollback()
}

func TestSQLiteRestoreVersionsConcurrentSnapshots(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	initial := restoreVersionRead(t, st)
	var wg sync.WaitGroup
	wg.Add(1)
	errs := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			tx, err := st.db.BeginTx(t.Context(), nil)
			if err != nil {
				errs <- err
				return
			}
			if _, err = tx.Exec(`UPDATE tasks SET goal='b'; UPDATE tasks SET goal='secret-goal'`); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			if err = tx.Commit(); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		out := restoreVersionRead(t, st)
		if (out.Epoch-initial.Epoch)%2 != 0 || out.DataSHA256 != initial.DataSHA256 {
			t.Fatal("partial business/journal transaction")
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestSQLiteRestoreVersionsCrashHelper(t *testing.T) {
	mode := os.Getenv("AI_AGENT_ROW_VERSION_CRASH_MODE")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	st, err := NewSQLiteStoreWithRestoreVersions(os.Getenv("AI_AGENT_ROW_VERSION_CRASH_PATH"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	// Record the source's immutable pin before exercising a business transaction.
	zero := restoreVersionRead(t, st)
	pinData := strings.Join([]string{zero.Origin, zero.NamespaceSHA256, zero.SchemaSHA256, zero.EventsSHA256}, "\n")
	if err := os.WriteFile(st.restoreVersions.path+".pin", []byte(pinData), 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions VALUES('session','tenant','private','active',0,?,?)`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if mode == "after" {
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	os.Exit(23)
}
func TestSQLiteRestoreVersionsCrashDurability(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "crash.sqlite")
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteRestoreVersionsCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "AI_AGENT_ROW_VERSION_CRASH_MODE="+mode, "AI_AGENT_ROW_VERSION_CRASH_PATH="+path)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("crash helper: %v %s", err, output)
			}
			// A precommit crash leaves a hot journal; read-only opening must refuse it.
			// Let SQLite recover this dedicated fixture using its normal writable open.
			db, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path + ".pin")
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(string(raw), "\n")
			if len(parts) != 4 {
				t.Fatal("pin")
			}
			pin := SQLiteRestoreVersionPin{Origin: parts[0], NamespaceSHA256: parts[1], SchemaSHA256: parts[2], PrefixEventsSHA256: parts[3]}
			reader, err := OpenSQLiteRestoreVersionReader(path, pin)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			out, err := reader.ReadSQLiteRestoreVersions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			expected := int64(0)
			if mode == "after" {
				expected = 1
			}
			if out.Epoch != expected || int64(len(out.Versions)) != expected {
				t.Fatal("business/version durability diverged")
			}
		})
	}
}

func TestSQLiteRestoreVersionsCanceledBusinessTransaction(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	before := restoreVersionRead(t, st)
	ctx, cancel := context.WithCancel(t.Context())
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET goal='cancelled'`); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Commit(); err == nil {
		t.Fatal("cancelled transaction committed")
	}
	// database/sql rolls back cancelled transactions asynchronously.
	connection, err := st.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if out := restoreVersionRead(t, st); !reflect.DeepEqual(out, before) {
		t.Fatal("cancel retained business/version change")
	}
}

func TestSQLiteRestoreVersionsResourceCapacity(t *testing.T) {
	st := restoreVersionStore(t)
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < sqliteRestoreVersionMaxRows; i++ {
		if _, err := tx.Exec(`INSERT INTO sessions VALUES(?,'tenant','private','active',0,?,?)`, fmt.Sprintf("s%04d", i), time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := restoreVersionRead(t, st)
	if err := st.CreateSession(t.Context(), &types.Session{ID: "over-capacity", TenantID: "tenant"}); err == nil {
		t.Fatal("resource capacity accepted")
	}
	after := restoreVersionRead(t, st)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("resource overflow leaked business/epoch")
	}
}

func TestSQLiteRestoreVersionsFileGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions fixture")
	}
	st := restoreVersionStore(t)
	path := st.restoreVersions.path
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("public file accepted", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("public parent accepted", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	pin := restoreVersionPin(restoreVersionRead(t, st))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-journal", []byte("retain-for-inspection"), 0600); err != nil {
		t.Fatal(err)
	}
	if reader, err := OpenSQLiteRestoreVersionReader(path, pin); err == nil {
		reader.Close()
		t.Fatal("journal accepted by readonly reader")
	}
	if body, err := os.ReadFile(path + "-journal"); err != nil || string(body) != "retain-for-inspection" {
		t.Fatal("sidecar changed", err)
	}
}
