package api

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type streamProjectionOnlyStore struct {
	store.Store
	calls       int
	unavailable bool
}

func (s *streamProjectionOnlyStore) GetTask(context.Context, string) (*types.Task, error) {
	return nil, errors.New("full Trace reads must not be used")
}
func (s *streamProjectionOnlyStore) GetTaskStreamSnapshot(context.Context, string) (store.TaskStreamSnapshot, error) {
	s.calls++
	if s.unavailable {
		return store.TaskStreamSnapshot{}, errors.New("projection temporarily unavailable")
	}
	return store.TaskStreamSnapshot{ID: "projection-only", Status: types.StatusPartial, FinalAnswer: "answer", TraceCount: 10000, StepCount: 3, TokenUsage: types.TokenUsage{TotalTokens: 999}}, nil
}
func TestSSEUsesLightweightSnapshotWithoutFullTraceFallback(t *testing.T) {
	st := &streamProjectionOnlyStore{Store: store.NewMemoryStore()}
	h := &Handler{store: st}
	snapshot, err := h.readTaskStreamSnapshot(t.Context(), "projection-only")
	if err != nil {
		t.Fatal(err)
	}
	e := streamSnapshotEvent(snapshot)
	if e.Final != "answer" || e.TokenUsage.TotalTokens != 999 || e.Snapshot.TraceCount != 10000 || e.Snapshot.StepCount != 3 || !e.isTerminal() {
		t.Fatal("projection changed SSE payload")
	}
	st.unavailable = true
	if _, err := h.readTaskStreamSnapshot(t.Context(), "projection-only"); err == nil {
		t.Fatal("projection failure accepted")
	}
	if st.calls != 2 {
		t.Fatal("projection was bypassed")
	}
}
