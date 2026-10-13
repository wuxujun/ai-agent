package store

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExternalRedisTaskStreamSnapshot(t *testing.T) {
	requireExternalIntegration(t)
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	st, err := NewRedisStoreFromURL(url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkTaskStreamSnapshot(t, st)
	task := &types.Task{ID: "stream-legacy-contract-" + uuid.NewString(), Status: types.StatusRunning, Trace: []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 71}}}}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteTask(t.Context(), task.ID)
	metadata, err := st.client.Get(t.Context(), st.taskMetadataKey(task.ID)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(metadata, &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "stream_stats")
	legacy, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), st.taskMetadataKey(task.ID), legacy, 0).Err(); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTaskStreamSnapshot(t.Context(), task.ID)
	if err != nil || got.TraceCount != 1 || got.TokenUsage.TotalTokens != 71 {
		t.Fatalf("legacy snapshot: %+v %v", got, err)
	}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	// Poison only the full JSON in this dedicated fixture. A modern snapshot
	// must read its atomic metadata rather than deserialize Trace JSON.
	raw, err := st.client.Get(t.Context(), st.taskKey(task.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), st.taskKey(task.ID), "invalid-full-json", 0).Err(); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetTaskStreamSnapshot(t.Context(), task.ID)
	if restore := st.client.Set(t.Context(), st.taskKey(task.ID), raw, 0).Err(); restore != nil {
		t.Fatal(restore)
	}
	if err != nil || got.TraceCount != 1 || got.TokenUsage.TotalTokens != 71 {
		t.Fatalf("modern metadata read fell back: %v", err)
	}
}
func TestExternalPostgresTaskStreamSnapshot(t *testing.T) {
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
	checkTaskStreamSnapshot(t, st)
}
