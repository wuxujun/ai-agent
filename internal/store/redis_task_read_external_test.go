package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExternalRedisTaskReadModel_PaginationTransitionRepairAndDelete(t *testing.T) {
	requireExternalIntegration(t)
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	st, err := NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	prefix := "task-read-" + uuid.NewString()
	tenant := prefix + "-tenant"
	created := time.Now().UTC().Truncate(time.Microsecond)
	for _, suffix := range []string{"a", "b"} {
		id := prefix + "-" + suffix
		task := &types.Task{ID: id, TenantID: tenant, SessionID: "session-1", CreatedAt: created,
			Goal: "test", Status: types.StatusCreated,
			Trace: []types.StepTrace{{Step: 1, Action: "one"}, {Step: 1, Action: "two"}, {Step: 2, Action: "three"}}}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), id) })
	}
	first, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, Status: types.StatusCreated, Limit: 1})
	if err != nil || len(first) != 1 || first[0].ID != prefix+"-b" {
		t.Fatalf("summary first = %+v, %v", first, err)
	}
	second, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, Status: types.StatusCreated,
		BeforeCreatedAt: first[0].CreatedAt, BeforeID: first[0].ID, Limit: 2})
	if err != nil || len(second) != 1 || second[0].ID != prefix+"-a" {
		t.Fatalf("summary second = %+v, %v", second, err)
	}
	metadata, err := st.GetTaskWithoutTrace(ctx, prefix+"-a")
	if err != nil || len(metadata.Trace) != 0 || metadata.Status != types.StatusCreated {
		t.Fatalf("metadata = %+v, %v", metadata, err)
	}
	page1, err := st.ListTaskTraces(ctx, prefix+"-a", 0, 2)
	if err != nil || len(page1) != 2 || page1[0].Sequence != 1 || page1[1].Sequence != 2 || page1[1].Trace.Step != 1 {
		t.Fatalf("trace first = %+v, %v", page1, err)
	}
	if page1[0].RecordedAt == nil || page1[1].RecordedAt == nil {
		t.Fatalf("new Redis trace timestamps missing: %+v", page1)
	}
	page2, err := st.ListTaskTraces(ctx, prefix+"-a", 2, 2)
	if err != nil || len(page2) != 1 || page2[0].Trace.Action != "three" {
		t.Fatalf("trace second = %+v, %v", page2, err)
	}
	changed, err := st.TryTransitionTaskStatus(ctx, prefix+"-a", []types.TaskStatus{types.StatusCreated}, types.StatusRunning)
	if err != nil || !changed {
		t.Fatalf("transition = %v, %v", changed, err)
	}
	running, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, Status: types.StatusRunning, Limit: 2})
	if err != nil || len(running) != 1 || running[0].ID != prefix+"-a" {
		t.Fatalf("running summaries = %+v, %v", running, err)
	}
	metadata, err = st.GetTaskWithoutTrace(ctx, prefix+"-a")
	if err != nil || metadata.Status != types.StatusRunning {
		t.Fatalf("transition metadata = %+v, %v", metadata, err)
	}
	full, err := st.GetTask(ctx, prefix+"-a")
	if err != nil {
		t.Fatal(err)
	}
	full.Trace = append(full.Trace, types.StepTrace{Step: 2, Action: "four"})
	if err := st.SaveFullTask(ctx, full); err != nil {
		t.Fatal(err)
	}
	appended, err := st.ListTaskTraces(ctx, prefix+"-a", 2, 3)
	if err != nil || len(appended) != 2 || appended[1].Sequence != 4 || appended[1].Trace.Action != "four" {
		t.Fatalf("trace after snapshot save = %+v, %v", appended, err)
	}
	if appended[0].RecordedAt == nil || appended[1].RecordedAt == nil || !appended[0].RecordedAt.Equal(*page2[0].RecordedAt) {
		t.Fatalf("Redis trace timestamps changed on snapshot save: %+v", appended)
	}
	// Simulate a task written by the previous Redis format and repair only
	// this test task, leaving any shared migration marker untouched.
	member := taskSummaryMember(created, prefix+"-b")
	if err := st.client.Del(ctx, st.taskSummaryKey(prefix+"-b"), st.taskMetadataKey(prefix+"-b"), st.taskTraceKey(prefix+"-b"), st.taskTraceTimeKey(prefix+"-b")).Err(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{taskSummaryIndex, taskSummaryTenantBase + tenant,
		taskSummaryStatusBase + string(types.StatusCreated), taskSummaryTenantStatusBase + tenant + ":" + string(types.StatusCreated)} {
		if err := st.client.ZRem(ctx, index, member).Err(); err != nil {
			t.Fatal(err)
		}
	}
	legacyPage, err := st.ListTaskTraces(ctx, prefix+"-b", 1, 2)
	if err != nil || len(legacyPage) != 2 || legacyPage[0].Sequence != 2 {
		t.Fatalf("legacy trace fallback = %+v, %v", legacyPage, err)
	}
	if err := st.repairTaskReadModel(ctx, prefix+"-b"); err != nil {
		t.Fatal(err)
	}
	legacyAfterRepair, err := st.ListTaskTraces(ctx, prefix+"-b", 0, 1)
	if err != nil || len(legacyAfterRepair) != 1 || legacyAfterRepair[0].RecordedAt != nil {
		t.Fatalf("legacy trace received fabricated timestamp: %+v, %v", legacyAfterRepair, err)
	}
	repaired, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, Status: types.StatusCreated, Limit: 2})
	if err != nil || len(repaired) != 1 || repaired[0].ID != prefix+"-b" {
		t.Fatalf("repaired summaries = %+v, %v", repaired, err)
	}
	deleted, err := st.DeleteTask(ctx, prefix+"-b")
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	keys, err := st.client.Exists(ctx, st.taskSummaryKey(prefix+"-b"), st.taskMetadataKey(prefix+"-b"), st.taskTraceKey(prefix+"-b"), st.taskTraceTimeKey(prefix+"-b")).Result()
	if err != nil || keys != 0 {
		t.Fatalf("deleted read model key count = %d, %v", keys, err)
	}
	remaining, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, Status: types.StatusCreated, Limit: 2})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("deleted summary remained = %+v, %v", remaining, err)
	}
}

func TestExternalRedisTaskReadModel_MigratesLegacyTask(t *testing.T) {
	requireExternalIntegration(t)
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	st, err := NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	id := "legacy-read-" + uuid.NewString()
	created := time.Now().UTC().Truncate(time.Microsecond)
	task := &types.Task{ID: id, TenantID: id + "-tenant", CreatedAt: created, Goal: "legacy",
		Status: types.StatusCreated, Trace: []types.StepTrace{{Step: 1, Action: "legacy-event"}}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), id) })
	member := taskSummaryMember(created, id)
	if err := st.client.Del(ctx, st.taskSummaryKey(id), st.taskMetadataKey(id), st.taskTraceKey(id), taskSummaryMigrationMarker).Err(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{taskSummaryIndex, taskSummaryTenantBase + task.TenantID,
		taskSummaryStatusBase + string(task.Status), taskSummaryTenantStatusBase + task.TenantID + ":" + string(task.Status)} {
		if err := st.client.ZRem(ctx, index, member).Err(); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: task.TenantID, Limit: 2})
	if err != nil || len(page) != 1 || page[0].ID != id {
		t.Fatalf("migrated summary = %+v, %v", page, err)
	}
	events, err := st.ListTaskTraces(ctx, id, 0, 2)
	if err != nil || len(events) != 1 || events[0].Trace.Action != "legacy-event" {
		t.Fatalf("migrated trace = %+v, %v", events, err)
	}
}

func TestExternalRedisTaskReadModel_SessionFilterAcrossIndexBatches(t *testing.T) {
	requireExternalIntegration(t)
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	st, err := NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	prefix := "session-read-" + uuid.NewString()
	tenant := prefix + "-tenant"
	created := time.Now().UTC().Truncate(time.Microsecond)
	var expected []string
	for i := 0; i < 205; i++ {
		id := prefix + "-" + fmt.Sprintf("%03d", i)
		session := "other"
		if i == 202 || i == 204 {
			session = "target"
			expected = append(expected, id)
		}
		if err := st.CreateTask(ctx, &types.Task{ID: id, TenantID: tenant, SessionID: session,
			CreatedAt: created.Add(-time.Duration(i) * time.Second), Status: types.StatusCreated}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), id) })
	}
	first, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, SessionID: "target", Limit: 1})
	if err != nil || len(first) != 1 || first[0].ID != expected[0] {
		t.Fatalf("first filtered page = %+v, %v", first, err)
	}
	second, err := st.ListTaskSummaries(ctx, TaskSummaryFilter{TenantID: tenant, SessionID: "target", Limit: 1,
		BeforeCreatedAt: first[0].CreatedAt, BeforeID: first[0].ID})
	if err != nil || len(second) != 1 || second[0].ID != expected[1] {
		t.Fatalf("second filtered page = %+v, %v", second, err)
	}
}
