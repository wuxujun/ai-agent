package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExternalRedisRetentionIsReadOnly(t *testing.T) {
	requireExternalIntegration(t)
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	st, err := NewRedisStoreFromURL(address)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	tenant := "retention-" + uuid.NewString()
	task := &types.Task{ID: tenant + "-task", TenantID: tenant, Status: types.StatusFailed, Goal: "private-goal"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteTask(ctx, task.ID)
	// Another contract may already have migrated this dedicated database.
	// Temporarily move only the marker under a unique fixture key, then restore
	// it before exercising the migrated view. Never flush a shared database.
	backup := taskSummaryMigrationMarker + ":fixture:" + tenant
	moved, err := st.client.Exists(ctx, taskSummaryMigrationMarker).Result()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		if err := st.client.Rename(ctx, taskSummaryMigrationMarker, backup).Err(); err != nil {
			t.Fatal(err)
		}
	}
	restored := false
	restoreMarker := func() {
		if moved != 0 && !restored {
			if err := st.client.Rename(ctx, backup, taskSummaryMigrationMarker).Err(); err != nil {
				t.Error(err)
				return
			}
		}
		restored = true
	}
	defer restoreMarker()
	snapshot := func() map[string]string { return jointRedisDump(t, st, task.ID) }
	before := snapshot()
	// No call to ensureTaskSummaryIndexes: missing markers must fail closed.
	if _, err := st.ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: tenant, Limit: 1}); !errors.Is(err, ErrRetentionReadModelUnavailable) {
		t.Fatalf("unmigrated view: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read-only scan migrated/repaired keys")
	}
	restoreMarker()
	if err := st.ensureTaskSummaryIndexes(ctx); err != nil {
		t.Fatal(err)
	} // Fixture setup only.
	before = snapshot()
	items, err := st.ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: tenant, Limit: 1})
	if err != nil || len(items) != 1 || items[0].TenantID != tenant || items[0].ID != task.ID || items[0].LeaseActive == nil || *items[0].LeaseActive || items[0].ApprovalsUnresolved != nil || items[0].TraceCount != nil {
		t.Fatalf("readonly metadata: %+v %v", items, err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("assessment wrote Redis data")
	}
	if ok, err := st.AcquireTaskLease(ctx, task.ID, "worker", time.Minute); !ok || err != nil {
		t.Fatal(err)
	}
	items, err = st.ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: tenant, Limit: 1})
	if err != nil || items[0].LeaseActive == nil || !*items[0].LeaseActive {
		t.Fatal("lease observation missing")
	}
	if err := st.client.Persist(ctx, "task:lease:"+task.ID).Err(); err != nil {
		t.Fatal(err)
	} // Corrupt TTL in fixture only.
	items, err = st.ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: tenant, Limit: 1})
	if err != nil || items[0].LeaseActive != nil {
		t.Fatal("unbounded lease treated as inactive")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.ListTaskRetentionObservations(canceled, TaskSummaryFilter{TenantID: tenant, Limit: 1}); err == nil {
		t.Fatal("canceled scan accepted")
	}
}
