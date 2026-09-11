package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskLeaseGuardContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		st := NewMemoryStore()
		t.Cleanup(func() { _ = st.Close() })
		runTaskLeaseGuardContract(t, st)
	})
	t.Run("sqlite", func(t *testing.T) {
		st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tasks.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		runTaskLeaseGuardContract(t, st)
	})
}

func runTaskLeaseGuardContract(t *testing.T, st Store) {
	t.Helper()
	id := "lease-guard-" + uuid.NewString()
	t.Cleanup(func() { _, _ = st.(TaskDeletionStore).DeleteTask(context.Background(), id) })
	task := &types.Task{ID: id, TenantID: "tenant-a", Goal: "original", Status: types.StatusCreated}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	t.Run("scoped_save_requires_matching_live_lease", func(t *testing.T) {
		before, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		stale := types.CloneTask(before)
		stale.Goal = "stale write"
		if err := st.SaveFullTask(WithTaskLease(t.Context(), id, "missing"), stale); !errors.Is(err, ErrTaskLeaseLost) {
			t.Fatalf("missing lease save error = %v, want ErrTaskLeaseLost", err)
		}
		if ok, err := st.AcquireTaskLease(t.Context(), id, "old-owner", time.Minute); err != nil || !ok {
			t.Fatalf("acquire: %v, %v", ok, err)
		}
		for _, ctx := range []context.Context{
			WithTaskLease(t.Context(), id, "wrong-owner"),
			WithTaskLease(t.Context(), id+"-different-task", "old-owner"),
			WithTaskLease(t.Context(), id, ""),
		} {
			if err := st.SaveFullTask(ctx, stale); !errors.Is(err, ErrTaskLeaseLost) {
				t.Fatalf("invalid lease save error = %v, want ErrTaskLeaseLost", err)
			}
		}
		after, err := st.GetTask(t.Context(), id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected save changed task: %+v, %v", after, err)
		}
		if err := st.ReleaseTaskLease(t.Context(), id, "old-owner"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("renew_never_reacquires_expired_or_replaced_lease", func(t *testing.T) {
		renewer, ok := st.(TaskLeaseStore)
		if !ok {
			t.Fatal("store does not support strict lease renewal")
		}
		if ok, err := renewer.RenewTaskLease(t.Context(), id, "missing", time.Minute); err != nil || ok {
			t.Fatalf("renew missing lease = %v, %v", ok, err)
		}
		if ok, err := st.AcquireTaskLease(t.Context(), id, "expired-owner", 20*time.Millisecond); err != nil || !ok {
			t.Fatalf("acquire: %v, %v", ok, err)
		}
		if ok, err := renewer.RenewTaskLease(t.Context(), id, "wrong-owner", time.Minute); err != nil || ok {
			t.Fatalf("renew wrong owner = %v, %v", ok, err)
		}
		time.Sleep(30 * time.Millisecond)
		if ok, err := renewer.RenewTaskLease(t.Context(), id, "expired-owner", time.Minute); err != nil || ok {
			t.Fatalf("renew expired lease = %v, %v", ok, err)
		}
		stale, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		stale.Goal = "stale write"
		if err := st.SaveFullTask(WithTaskLease(t.Context(), id, "expired-owner"), stale); !errors.Is(err, ErrTaskLeaseLost) {
			t.Fatalf("expired lease save error = %v", err)
		}
		if ok, err := st.AcquireTaskLease(t.Context(), id, "fresh-owner", time.Minute); err != nil || !ok {
			t.Fatalf("fresh acquisition = %v, %v", ok, err)
		}
		if ok, err := renewer.RenewTaskLease(t.Context(), id, "expired-owner", time.Minute); err != nil || ok {
			t.Fatalf("renew replaced lease = %v, %v", ok, err)
		}
		if ok, err := renewer.RenewTaskLease(t.Context(), id, "fresh-owner", time.Minute); err != nil || !ok {
			t.Fatalf("renew live lease = %v, %v", ok, err)
		}
		fresh := types.CloneTask(stale)
		fresh.Goal = "fresh result"
		fresh.Trace = []types.StepTrace{{Step: 1, Action: "reason", Observation: "fresh"}}
		if err := st.SaveFullTask(WithTaskLease(t.Context(), id, "fresh-owner"), fresh); err != nil {
			t.Fatalf("fresh save: %v", err)
		}
		before, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(WithTaskLease(t.Context(), id, "expired-owner"))
		cancel()
		if err := st.SaveFullTask(context.WithoutCancel(ctx), stale); !errors.Is(err, ErrTaskLeaseLost) {
			t.Fatalf("stale flush error = %v", err)
		}
		if ok, err := st.TryTransitionTaskStatus(context.WithoutCancel(ctx), id, []types.TaskStatus{types.StatusCreated}, types.StatusPaused); ok || !errors.Is(err, ErrTaskLeaseLost) {
			t.Fatalf("stale status transition = %v, %v", ok, err)
		}
		after, err := st.GetTask(t.Context(), id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("stale flush replaced fresh task: %+v, %v", after, err)
		}
	})
}
