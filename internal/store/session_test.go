package store

import (
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteSessionTaskAndMemoryPersistence(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	if err := st.CreateSession(ctx, &types.Session{ID: "session-a", TenantID: "tenant-a", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	sequence, err := st.NextSessionTaskSequence(ctx, "session-a", "tenant-a")
	if err != nil || sequence != 1 {
		t.Fatalf("sequence=%d err=%v", sequence, err)
	}
	task := &types.Task{ID: "task-a", TenantID: "tenant-a", SessionID: "session-a", SequenceNo: sequence, Goal: "goal", Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1}
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionID != "session-a" || loaded.SequenceNo != 1 || loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("task session fields not persisted: %+v", loaded)
	}
	if err := st.SaveMemory(ctx, &types.Memory{ID: "memory-a", TenantID: "tenant-a", SessionID: "session-a", TaskID: task.ID, Goal: "goal"}); err != nil {
		t.Fatal(err)
	}
	items, err := st.QueryMemories(WithSessionScope(WithTenantScope(ctx, "tenant-a"), "session-a"), "goal", nil, 5)
	if err != nil || len(items) != 1 || items[0].SessionID != "session-a" {
		t.Fatalf("session memories=%+v err=%v", items, err)
	}
}
