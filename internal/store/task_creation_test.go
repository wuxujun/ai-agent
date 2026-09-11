package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskCreationContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		st := NewMemoryStore()
		t.Cleanup(func() { _ = st.Close() })
		runTaskCreationContract(t, st)
	})
	t.Run("sqlite", func(t *testing.T) {
		st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tasks.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		runTaskCreationContract(t, st)
	})
}

func runTaskCreationContract(t *testing.T, st Store) {
	t.Helper()
	creator, ok := st.(TaskCreationStore)
	if !ok {
		t.Fatal("store does not support atomic task creation")
	}
	prefix := "create-contract-" + uuid.NewString()
	newTask := func(id, tenant string) *types.Task {
		return &types.Task{
			ID: id, TenantID: tenant, SessionID: prefix + "-session", SequenceNo: 7,
			Goal: tenant + " original goal", Status: types.StatusCreated, MaxSteps: 8,
			BrainProjectID: "project", BrainSnapshotID: "snapshot", BrainConfigDigest: "digest",
			Trace: []types.StepTrace{{Step: 1, Action: "reason", Observation: tenant}},
		}
	}
	cleanupTask := func(id string) {
		t.Cleanup(func() {
			if _, err := st.(TaskDeletionStore).DeleteTask(context.Background(), id); err != nil {
				t.Errorf("clean up task: %v", err)
			}
		})
	}
	t.Run("duplicate_preserves_entire_task", func(t *testing.T) {
		id := prefix + "-duplicate"
		cleanupTask(id)
		original := newTask(id, prefix+"-tenant-a")
		if err := creator.CreateTask(t.Context(), original); err != nil {
			t.Fatal(err)
		}
		before, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if before.Goal != original.Goal || before.TenantID != original.TenantID || before.SessionID != original.SessionID || before.SequenceNo != 7 || before.BrainSnapshotID != "snapshot" || len(before.Trace) != 1 || before.Trace[0].Observation != original.TenantID {
			t.Fatalf("task was not completely persisted: %+v", before)
		}
		for _, tenant := range []string{original.TenantID, prefix + "-tenant-b"} {
			duplicate := newTask(id, tenant)
			duplicate.Goal = "replacement"
			duplicate.Status = types.StatusRunning
			duplicate.Trace = nil
			if err := creator.CreateTask(t.Context(), duplicate); !errors.Is(err, ErrTaskExists) {
				t.Fatalf("duplicate creation error = %v, want ErrTaskExists", err)
			}
			after, err := st.GetTask(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("duplicate changed persisted task: before=%+v after=%+v", before, after)
			}
		}
		tasks, err := st.ListTasks(t.Context(), ListFilter{TenantID: prefix + "-tenant-b", Status: types.StatusRunning})
		if err != nil || len(tasks) != 0 {
			t.Fatalf("duplicate polluted tenant/status index: tasks=%+v err=%v", tasks, err)
		}
		before.Goal = "legitimate update"
		if err := st.SaveFullTask(t.Context(), before); err != nil {
			t.Fatalf("existing update path failed: %v", err)
		}
		updated, err := st.GetTask(t.Context(), id)
		if err != nil || updated.Goal != "legitimate update" {
			t.Fatalf("update did not persist: task=%+v err=%v", updated, err)
		}
	})
	t.Run("concurrent_creation_has_one_winner", func(t *testing.T) {
		id := prefix + "-concurrent"
		cleanupTask(id)
		const writers = 16
		results := make([]error, writers)
		tasks := make([]*types.Task, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range writers {
			tasks[i] = newTask(id, fmt.Sprintf("%s-tenant-%d", prefix, i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = creator.CreateTask(t.Context(), tasks[i])
			}()
		}
		close(start)
		wg.Wait()
		winner := -1
		for i, err := range results {
			if err == nil {
				if winner != -1 {
					t.Fatalf("multiple successful creators: %d and %d", winner, i)
				}
				winner = i
			} else if !errors.Is(err, ErrTaskExists) {
				t.Errorf("losing creator returned %v, want ErrTaskExists", err)
			}
		}
		if winner == -1 {
			t.Fatalf("no successful creator: %v", results)
		}
		got, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.TenantID != tasks[winner].TenantID || got.Goal != tasks[winner].Goal || len(got.Trace) != 1 || got.Trace[0].Observation != tasks[winner].TenantID {
			t.Fatalf("persisted task does not belong to successful creator: %+v", got)
		}
	})
	t.Run("canceled_creation_does_not_persist", func(t *testing.T) {
		id := prefix + "-canceled"
		cleanupTask(id)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := creator.CreateTask(ctx, newTask(id, prefix)); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled creation error = %v", err)
		}
		if exists, err := st.ExistsTask(t.Context(), id); err != nil || exists {
			t.Fatalf("canceled creation persisted task: exists=%v err=%v", exists, err)
		}
	})
}
