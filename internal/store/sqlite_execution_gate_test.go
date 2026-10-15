package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func executionFixture(t *testing.T) (*SQLiteStore, int64) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := NewSQLiteStoreWithExecutionGate(filepath.Join(dir, "execution.sqlite"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	restoreVersionTask(t, st)
	epoch, err := st.EnrollSQLiteExecutionGate(t.Context(), "task", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	return st, epoch
}
func executionSnapshot(t *testing.T, st *SQLiteStore) *SQLiteExecutionGateSnapshot {
	t.Helper()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func executionStart(t *testing.T, st *SQLiteStore, epoch int64, id string, ttl time.Duration) *SQLiteExecutionAttempt {
	t.Helper()
	a, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", id, strings.Repeat("b", 64), epoch, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSQLiteExecutionGateLifecycle(t *testing.T) {
	st, epoch := executionFixture(t)
	before := executionSnapshot(t, st)
	if before.Status != "unverified" || before.Phase != "open" || before.ReceiptsComplete {
		t.Fatal("unsafe initial state")
	}
	a := executionStart(t, st, epoch, "attempt", time.Hour)
	owner := "execution-attempt:" + a.ID
	if err := st.ReleaseTaskLease(t.Context(), "task", owner); err == nil {
		t.Fatal("release before receipts accepted")
	}
	if _, err := st.DeleteTask(t.Context(), "task"); err == nil {
		t.Fatal("unresolved attempt task removed")
	}
	blocked, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", epoch, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", "new", a.InstanceSHA256, blocked, time.Hour); !errors.Is(err, ErrSQLiteExecutionBlocked) {
		t.Fatal("blocked dispatch", err)
	}
	if ok, err := st.RenewTaskLease(t.Context(), "task", owner, time.Hour); err != nil || !ok {
		t.Fatal("drain ownership could not renew", err)
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("d", 64), strings.Repeat("e", 64)); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("finalized before exit", err)
	}
	if err := st.ExitSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	exited := executionSnapshot(t, st)
	if exited.Attempts[0].State != "exited" || exited.ReceiptsComplete || exited.LeaseState != "active-unresolved" {
		t.Fatal("exit inferred cleanup")
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("d", 64), ""); err == nil {
		t.Fatal("missing cleanup accepted")
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("d", 64), strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	final := executionSnapshot(t, st)
	if !final.ReceiptsComplete || final.Status != "unverified" || final.LeaseState != "absent" || final.Attempts[0].State != "complete" {
		t.Fatal("final receipt state")
	}
	// Exact receipt retries cannot replay execution, release another owner or
	// advance epochs. Conflicting retries and unknown instances are rejected.
	if err := st.ExitSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("d", 64), strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, executionSnapshot(t, st)) {
		t.Fatal("receipt replay changed state")
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("f", 64), strings.Repeat("e", 64)); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("conflicting receipt accepted", err)
	}
	draining, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", blocked, true)
	if err != nil || draining <= blocked {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE execution_gates SET phase='open'`); err == nil {
		t.Fatal("gate reopened")
	}
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", draining, false); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("drain regressed", err)
	}
}

func TestSQLiteExecutionGateCannotBypassWithLeaseOrExpiry(t *testing.T) {
	st, epoch := executionFixture(t)
	if ok, err := st.AcquireTaskLease(t.Context(), "task", "ordinary-owner", time.Hour); err == nil || ok {
		t.Fatal("ordinary lease bypassed attempt registration")
	}
	a := executionStart(t, st, epoch, "short", time.Nanosecond)
	if ok, err := st.RenewTaskLease(t.Context(), "task", "execution-attempt:"+a.ID, time.Hour); err != nil || ok {
		t.Fatal("expired lease revived", err)
	}
	out := executionSnapshot(t, st)
	if out.LeaseState != "expired-unresolved" || out.ReceiptsComplete || out.Attempts[0].State != "running" {
		t.Fatal("expiry treated as stop")
	}
	if _, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", "another", a.InstanceSHA256, epoch, time.Hour); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("expired unresolved attempt replaced", err)
	}
	if _, err := st.db.Exec(`UPDATE task_leases SET expires_at=? WHERE task_id='task'`, time.Now().Add(time.Hour).UnixNano()); err == nil {
		t.Fatal("raw SQL revived ownership")
	}
	if _, err := st.EnrollSQLiteExecutionGate(t.Context(), "task", "tenant"); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("gate reenrolled", err)
	}
}

func TestSQLiteExecutionGateAtomicCompetition(t *testing.T) {
	for i := 0; i < 12; i++ {
		st, epoch := executionFixture(t)
		ready := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var attempt *SQLiteExecutionAttempt
		var startErr, blockErr error
		go func() {
			defer wg.Done()
			<-ready
			attempt, startErr = st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", "race", strings.Repeat("b", 64), epoch, time.Hour)
		}()
		go func() {
			defer wg.Done()
			<-ready
			_, blockErr = st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", epoch, false)
		}()
		close(ready)
		wg.Wait()
		if blockErr != nil {
			t.Fatal(blockErr)
		}
		out := executionSnapshot(t, st)
		if out.Phase != "blocked" {
			t.Fatal("block lost")
		}
		if startErr == nil {
			if attempt == nil || len(out.Attempts) != 1 || out.LeaseState != "active-unresolved" {
				t.Fatal("partial acquisition")
			}
		} else if !errors.Is(startErr, ErrSQLiteExecutionBlocked) || len(out.Attempts) != 0 || out.LeaseState != "absent" {
			t.Fatal("failed start leaked lease/attempt", startErr)
		}
	}
}

func TestSQLiteExecutionGateRollbackAndCAS(t *testing.T) {
	st, epoch := executionFixture(t)
	before := executionSnapshot(t, st)
	coreBefore := restoreVersionRead(t, st)
	if _, err := st.db.Exec(`CREATE TRIGGER execution_fail_start BEFORE INSERT ON execution_events WHEN NEW.operation='start' BEGIN SELECT RAISE(ABORT,'fixture failure'); END;`); err != nil {
		t.Fatal(err)
	}
	// Incorporate only this explicit failure-injection schema into the test pin.
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	st.restoreVersions.schema, err = sqliteRestoreVersionSchema(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", "failed", strings.Repeat("b", 64), epoch, time.Hour); err == nil {
		t.Fatal("injected failure accepted")
	}
	after := executionSnapshot(t, st)
	coreAfter := restoreVersionRead(t, st)
	if !reflect.DeepEqual(before, after) || coreBefore.Epoch != coreAfter.Epoch || coreBefore.EventsSHA256 != coreAfter.EventsSHA256 || coreBefore.DataSHA256 != coreAfter.DataSHA256 {
		t.Fatal("partial business/control transaction")
	}
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", epoch+1, false); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	if _, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "other", "failed", strings.Repeat("b", 64), epoch, time.Hour); !errors.Is(err, ErrSQLiteExecutionBlocked) {
		t.Fatal("tenant mismatch accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.BlockSQLiteExecutionGate(ctx, "task", "tenant", epoch, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSQLiteExecutionGateReadOnlyPersistence(t *testing.T) {
	st, epoch := executionFixture(t)
	executionStart(t, st, epoch, "persistent", time.Hour)
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", epoch, true); err != nil {
		t.Fatal(err)
	}
	before := executionSnapshot(t, st)
	pin := restoreVersionPin(restoreVersionRead(t, st))
	path := st.restoreVersions.path
	if _, err := st.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, executionSnapshot(t, st)) {
		t.Fatal("readonly snapshot changed")
	}
	st.Close()
	reader, err := OpenSQLiteExecutionGateReader(path, pin)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	after, err := reader.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("persistent gate/attempt lost", err)
	}
	if _, err := reader.reader.store.db.Exec(`DELETE FROM execution_attempts`); err == nil {
		t.Fatal("reader writable")
	}
}

func TestSQLiteExecutionGateCorruptionAndTaskABA(t *testing.T) {
	st, epoch := executionFixture(t)
	executionStart(t, st, epoch, "active", time.Hour)
	if _, err := st.db.Exec(`DELETE FROM execution_events WHERE operation='start'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant"); !errors.Is(err, ErrSQLiteExecutionGate) {
		t.Fatal("missing control event accepted", err)
	}
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "task", "tenant", epoch, false); !errors.Is(err, ErrSQLiteExecutionGate) {
		t.Fatal("corrupt control accepted mutation", err)
	}
	st, epoch = executionFixture(t)
	original := restoreVersionTask(t, st)
	if ok, err := st.DeleteTask(t.Context(), "task"); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.CreateTask(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartSQLiteExecutionAttempt(t.Context(), "task", "tenant", "recreated", strings.Repeat("b", 64), epoch, time.Hour); !errors.Is(err, ErrSQLiteExecutionBlocked) {
		t.Fatal("task ABA reused enrolled gate", err)
	}
	if _, err := st.EnrollSQLiteExecutionGate(t.Context(), "task", "tenant"); !errors.Is(err, ErrSQLiteExecutionConflict) {
		t.Fatal("ABA gate automatically re-enrolled", err)
	}
}

func TestSQLiteExecutionGateFinalizeRequiresCurrentOwnership(t *testing.T) {
	st, epoch := executionFixture(t)
	a := executionStart(t, st, epoch, "expired-finalize", time.Nanosecond)
	if err := st.ExitSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeSQLiteExecutionAttempt(t.Context(), "task", "tenant", a.ID, a.InstanceSHA256, strings.Repeat("d", 64), strings.Repeat("e", 64)); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatal("expired ownership finalized", err)
	}
	out := executionSnapshot(t, st)
	if out.Attempts[0].State != "exited" || out.LeaseState != "expired-unresolved" || out.ReceiptsComplete {
		t.Fatal("failed ownership check released evidence")
	}
}
