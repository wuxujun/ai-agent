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
	snapshot := func() map[string]string {
		t.Helper()
		keys, err := st.client.Keys(ctx, "*").Result()
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, key := range keys {
			value, err := st.client.Dump(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			result[key] = value
		}
		return result
	}
	before := snapshot()
	// No call to ensureTaskSummaryIndexes: missing markers must fail closed.
	if _, err := st.ListTaskRetentionObservations(ctx, TaskSummaryFilter{TenantID: tenant, Limit: 1}); !errors.Is(err, ErrRetentionReadModelUnavailable) {
		t.Fatalf("unmigrated view: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read-only scan migrated/repaired keys")
	}
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
