package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExternalRedisStatusTransitionMaintainsTenantIndexes(t *testing.T) {
	requireExternalIntegration(t)
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	st, err := NewRedisStoreFromURL(url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prefix := "status-contract-" + uuid.NewString()
	tenant := prefix + "-tenant"
	for _, suffix := range []string{"a", "b"} {
		task := &types.Task{ID: prefix + suffix, TenantID: tenant, Status: types.StatusCreated}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), task.ID) })
	}
	if _, err := st.ListTasks(t.Context(), ListFilter{TenantID: tenant, Status: types.StatusCreated}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"a", "b"} {
		if ok, err := st.TryTransitionTaskStatus(t.Context(), prefix+suffix, []types.TaskStatus{types.StatusCreated}, types.StatusRunning); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	for page := 0; page < 2; page++ {
		got, err := st.ListTasks(t.Context(), ListFilter{TenantID: tenant, Status: types.StatusRunning, Limit: 1, Offset: page})
		if err != nil || len(got) != 1 {
			t.Fatalf("running page %d: %+v %v", page, got, err)
		}
	}
	old, err := st.client.ZCard(t.Context(), taskTenantStatusIndexBase+tenant+":created").Result()
	if err != nil || old != 0 {
		t.Fatalf("stale created index has %d entries: %v", old, err)
	}
	task, err := st.GetTask(t.Context(), prefix+"a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	other, err := st.ListTasks(t.Context(), ListFilter{TenantID: tenant + "-other", Status: types.StatusRunning})
	if err != nil || len(other) != 0 {
		t.Fatal(other, err)
	}
	if ok, err := st.AcquireTaskLease(t.Context(), prefix+"a", "owner", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := st.TryTransitionTaskStatus(WithTaskLease(t.Context(), prefix+"a", "other"), prefix+"a", []types.TaskStatus{types.StatusRunning}, types.StatusPaused); err == nil || ok {
		t.Fatal("status index fix bypassed lease guard")
	}
}

func TestExternalRedisRebuildsLegacyStatusIndexes(t *testing.T) {
	requireExternalIntegration(t)
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	st, err := NewRedisStoreFromURL(url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := "legacy-index-" + uuid.NewString()
	tenant := id + "-tenant"
	task := &types.Task{ID: id, TenantID: tenant, Status: types.StatusRunning}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteTask(context.Background(), id)
	// An old binary has a migrated v3 marker, but its created index still contains
	// a running task and a deleted ghost. v4 must rebuild without using that data.
	oldIndex := "tasks:tenant_status:" + tenant + ":created"
	if err := st.client.ZAdd(t.Context(), oldIndex, redis.Z{Member: id}, redis.Z{Member: id + "-ghost"}).Err(); err != nil {
		t.Fatal(err)
	}
	defer st.client.Del(context.Background(), oldIndex)
	if err := st.client.ZRem(t.Context(), taskTenantStatusIndexBase+tenant+":running", id).Err(); err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), "tasks:index:v3:migrated", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := st.client.Del(t.Context(), tasksIndexV2Marker).Err(); err != nil {
		t.Fatal(err)
	}
	running, err := st.ListTasks(t.Context(), ListFilter{TenantID: tenant, Status: types.StatusRunning, Limit: 1})
	if err != nil || len(running) != 1 || running[0].ID != id {
		t.Fatalf("legacy task missing after rebuild: %+v %v", running, err)
	}
	created, err := st.ListTasks(t.Context(), ListFilter{TenantID: tenant, Status: types.StatusCreated, Limit: 1})
	if err != nil || len(created) != 0 {
		t.Fatalf("legacy ghost leaked into rebuilt index: %+v %v", created, err)
	}
}
