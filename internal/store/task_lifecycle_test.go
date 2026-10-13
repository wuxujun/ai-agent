package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskLifecycleContract(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "lifecycle.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			checkTaskLifecycle(t, st)
		})
	}
}

func checkTaskLifecycle(t *testing.T, st Store) {
	t.Helper()
	ctx := t.Context()
	oldTime := time.Now().Add(-365 * 24 * time.Hour)
	task := &types.Task{ID: "life-" + uuid.NewString(), TenantID: "life-tenant", Goal: "quote \" backslash \\ nested {[,]}", SequenceNo: (1 << 53) + 1, Status: types.StatusRunning, TerminalSince: &oldTime, LastMaintenanceAt: &oldTime, MaintenanceVersion: MaxMaintenanceVersion, Unresolved: []string{}, Trace: []types.StepTrace{{Step: 1, Action: "fixture", Evidence: []types.Evidence{}}}}
	if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	defer st.(TaskDeletionStore).DeleteTask(context.Background(), task.ID)
	read := func(version int64, terminal bool) *types.Task {
		t.Helper()
		got, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.MaintenanceVersion != version || got.LastMaintenanceAt == nil || (got.TerminalSince != nil) != terminal {
			t.Fatalf("lifecycle v%d terminal=%v: %+v", version, terminal, lifecycleOf(got))
		}
		if got.Goal != task.Goal || got.SequenceNo != task.SequenceNo {
			t.Fatal("lifecycle mutation changed payload/number precision")
		}
		return got
	}
	first := read(1, false)
	if task.MaintenanceVersion != 1 || task.TerminalSince != nil {
		t.Fatal("caller fields accepted")
	}
	if ok, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusPaused}, types.StatusFailed); ok || err != nil {
		t.Fatalf("nonmatching transition: %v %v", ok, err)
	}
	if !reflect.DeepEqual(lifecycleOf(first), lifecycleOf(read(1, false))) {
		t.Fatal("failed transition changed version")
	}
	if ok, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusRunning}, types.StatusFailed); !ok || err != nil {
		t.Fatal(err)
	}
	finished := read(2, true)
	if finished.TerminalSince.Before(*first.LastMaintenanceAt) {
		t.Fatal("terminal time preceded initial mutation")
	}
	finished.MaintenanceVersion = 0
	finished.TerminalSince = &oldTime
	finished.LastMaintenanceAt = &oldTime
	if err := st.SaveFullTask(ctx, finished); err != nil {
		t.Fatal(err)
	}
	saved := read(3, true)
	if !saved.TerminalSince.Equal(*finished.TerminalSince) || saved.TerminalSince.Equal(oldTime) || saved.LastMaintenanceAt.Before(*saved.TerminalSince) {
		t.Fatal("same-terminal save lost trusted time")
	}
	clone := types.CloneTask(saved)
	*clone.TerminalSince = oldTime
	*clone.LastMaintenanceAt = oldTime
	if !reflect.DeepEqual(lifecycleOf(saved), lifecycleOf(read(3, true))) {
		t.Fatal("lifecycle pointers alias store")
	}
	if sqlStore, ok := st.(interface {
		ReplaceTraces(context.Context, string, []types.StepTrace) error
		SaveTask(context.Context, *types.Task) error
	}); ok {
		if err := sqlStore.ReplaceTraces(ctx, task.ID, []types.StepTrace{{Step: 2, Action: "replacement"}}); err != nil {
			t.Fatal(err)
		}
		metadata := read(4, true)
		metadata.Trace = nil
		if err := sqlStore.SaveTask(ctx, metadata); err != nil {
			t.Fatal(err)
		}
		if len(read(5, true).Trace) != 1 {
			t.Fatal("metadata save replaced Trace")
		}
	}
	before, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.SaveFullTask(canceled, types.CloneTask(before)); err == nil {
		t.Fatal("canceled save accepted")
	}
	if ok, err := st.TryTransitionTaskStatus(canceled, task.ID, []types.TaskStatus{types.StatusFailed}, types.StatusRunning); ok || err == nil {
		t.Fatal("canceled transition accepted")
	}
	if !reflect.DeepEqual(lifecycleOf(before), lifecycleOf(read(before.MaintenanceVersion, true))) {
		t.Fatal("canceled mutation changed lifecycle")
	}
	if ok, err := st.AcquireTaskLease(ctx, task.ID, "other-owner", time.Minute); !ok || err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFullTask(WithTaskLease(ctx, task.ID, "wrong-owner"), before); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatalf("lease: %v", err)
	}
	if err := st.ReleaseTaskLease(ctx, task.ID, "other-owner"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lifecycleOf(before), lifecycleOf(read(before.MaintenanceVersion, true))) {
		t.Fatal("rejected save changed lifecycle")
	}
	if ok, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusFailed}, types.StatusRunning); !ok || err != nil {
		t.Fatal(err)
	}
	resumed := read(before.MaintenanceVersion+1, false)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Go(func() { errs <- st.SaveFullTask(ctx, types.CloneTask(resumed)) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	concurrent := read(resumed.MaintenanceVersion+12, false)
	if concurrent.LastMaintenanceAt.Before(*resumed.LastMaintenanceAt) {
		t.Fatal("maintenance clock regressed")
	}
	if ok, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusRunning}, types.StatusPartial); !ok || err != nil {
		t.Fatal(err)
	}
	final := read(concurrent.MaintenanceVersion+1, true)
	if final.TerminalSince.Before(*saved.TerminalSince) {
		t.Fatal("new terminal epoch regressed")
	}
	if r, ok := st.(*RedisStore); ok {
		if err := r.ensureTaskSummaryIndexes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.(TaskRetentionReader).ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: task.TenantID, Limit: 51})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == task.ID {
			found = true
			if row.MaintenanceVersion != final.MaintenanceVersion || row.TerminalSince == nil || !row.TerminalSince.Equal(*final.TerminalSince) || row.LastMaintenanceAt == nil || !row.LastMaintenanceAt.Equal(*final.LastMaintenanceAt) {
				t.Fatalf("retention lifecycle mismatch: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("missing retention projection")
	}
	data, err := json.Marshal(final)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if _, ok := view["maintenance_version"].(string); !ok {
		t.Fatal("version not JSON decimal string")
	}
}

func TestTaskLifecycleLegacyAndBounds(t *testing.T) {
	now := time.Now().UTC()
	legacy := &taskLifecycle{Status: types.StatusFailed}
	got, err := nextTaskLifecycle(legacy, types.StatusFailed, now)
	if err != nil || got.MaintenanceVersion != 1 || got.TerminalSince != nil {
		t.Fatal("legacy terminal age invented")
	}
	future := now.Add(time.Hour)
	previous := &taskLifecycle{Status: types.StatusFailed, TerminalSince: &now, LastMaintenanceAt: &future, MaintenanceVersion: MaxMaintenanceVersion - 1}
	got, err = nextTaskLifecycle(previous, types.StatusFailed, now)
	if err != nil || got.MaintenanceVersion != MaxMaintenanceVersion || !got.LastMaintenanceAt.Equal(future) || !got.TerminalSince.Equal(now) {
		t.Fatal("monotonic clock or exact version bound")
	}
	if _, err := nextTaskLifecycle(&got, types.StatusRunning, now); !errors.Is(err, ErrMaintenanceVersionExhausted) {
		t.Fatal("version overflow accepted")
	}
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "legacy.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			task := &types.Task{ID: "legacy", TenantID: "legacy", Status: types.StatusFailed}
			if err := st.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			inject := func(version int64) {
				t.Helper()
				switch s := st.(type) {
				case *MemoryStore:
					s.mu.Lock()
					s.tasks[task.ID].MaintenanceVersion = version
					s.tasks[task.ID].TerminalSince = nil
					s.tasks[task.ID].LastMaintenanceAt = nil
					s.mu.Unlock()
				case *SQLiteStore:
					if _, err := s.db.Exec(`UPDATE tasks SET maintenance_version=?,terminal_since=NULL,last_maintenance_at=NULL WHERE id=?`, version, task.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			inject(0)
			if err := st.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetTask(t.Context(), task.ID)
			if err != nil || got.TerminalSince != nil || got.MaintenanceVersion != 1 {
				t.Fatal("legacy persisted age invented", err)
			}
			inject(MaxMaintenanceVersion)
			before, _ := st.GetTask(t.Context(), task.ID)
			if err := st.SaveFullTask(t.Context(), task); !errors.Is(err, ErrMaintenanceVersionExhausted) {
				t.Fatal(err)
			}
			after, _ := st.GetTask(t.Context(), task.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("exhausted save changed task")
			}
		})
	}
}

func TestExternalRedisTaskLifecycle(t *testing.T) {
	requireExternalIntegration(t)
	if os.Getenv("TEST_REDIS_URL") == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	st, err := NewRedisStoreFromURL(os.Getenv("TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkTaskLifecycle(t, st)
	task := &types.Task{ID: "life-limit-" + uuid.NewString(), TenantID: "life-limit", Status: types.StatusFailed}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteTask(context.Background(), task.ID)
	task.MaintenanceVersion = MaxMaintenanceVersion
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), st.taskKey(task.ID), raw, 0).Err(); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		keys, err := st.client.Keys(t.Context(), "*").Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			value, err := st.client.Dump(t.Context(), key).Result()
			if err != nil {
				t.Fatal(err)
			}
			result[key] = value
		}
		return result
	}
	before := snapshot()
	if err := st.SaveFullTask(t.Context(), task); !errors.Is(err, ErrMaintenanceVersionExhausted) {
		t.Fatal(err)
	}
	if ok, err := st.TryTransitionTaskStatus(t.Context(), task.ID, []types.TaskStatus{types.StatusFailed}, types.StatusRunning); ok || !errors.Is(err, ErrMaintenanceVersionExhausted) {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("exhausted script mutated task/index")
	}
	// A legacy terminal task can gain a maintenance version without inventing age.
	task.MaintenanceVersion = 0
	task.TerminalSince = nil
	task.LastMaintenanceAt = nil
	raw, err = json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), st.taskKey(task.ID), raw, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.GetTask(t.Context(), task.ID)
	if err != nil || legacy.MaintenanceVersion != 1 || legacy.TerminalSince != nil {
		t.Fatalf("legacy lifecycle: %+v %v", legacy, err)
	}
}

func TestExternalPostgresTaskLifecycle(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN missing")
	}
	st, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkTaskLifecycle(t, st)
}
