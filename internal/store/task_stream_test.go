package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskStreamSnapshotConsistentAcrossTraceMutations(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "stream.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			checkTaskStreamSnapshot(t, st)
		})
	}
}
func checkTaskStreamSnapshot(t *testing.T, st Store) {
	t.Helper()
	ctx := t.Context()
	reader := st.(TaskStreamSnapshotStore)
	task := &types.Task{ID: "stream-contract-" + uuid.NewString(), TenantID: "a", Status: types.StatusRunning, StepCount: 9}
	traces := []types.StepTrace{{Step: 1, Observation: "private body", TokenUsage: types.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}, {Step: 1, TokenUsage: types.TokenUsage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11}}}
	if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		full, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reader.GetTaskStreamSnapshot(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, StreamSnapshotFromTask(full)) {
			t.Fatalf("snapshot mismatch: %+v", got)
		}
	}
	check()
	if replacer, ok := st.(interface {
		ReplaceTraces(context.Context, string, []types.StepTrace) error
	}); ok {
		if err := replacer.ReplaceTraces(ctx, task.ID, traces); err != nil {
			t.Fatal(err)
		}
		check()
	}
	for _, snapshot := range [][]types.StepTrace{traces, append(append([]types.StepTrace(nil), traces...), traces[0]), {traces[1], traces[0]}, {traces[1]}, nil} {
		task.Trace = snapshot
		if err := st.SaveFullTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		check()
	}
	if transitioned, err := st.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusRunning}, types.StatusAwaitingApproval); err != nil || !transitioned {
		t.Fatalf("status transition: %v", err)
	}
	check()
	task.Status = types.StatusPartial
	task.FinalAnswer = "confirmed"
	task.ErrorCode = "partial"
	task.ErrorMessage = "public"
	task.Trace = traces
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	check()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.GetTaskStreamSnapshot(canceled, task.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("snapshot ignores cancellation")
	}
	if _, err := st.(TaskDeletionStore).DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetTaskStreamSnapshot(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted snapshot remained: %v", err)
	}
}

func TestTaskStreamSnapshotConcurrentStateAndTraceAreOneObservation(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var writer Store = NewMemoryStore()
			var reader TaskStreamSnapshotStore = writer.(TaskStreamSnapshotStore)
			if backend == "sqlite" {
				path := filepath.Join(t.TempDir(), "concurrent.db")
				a, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				b, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				writer = a
				reader = b
			}
			a := &types.Task{ID: "concurrent-stream", Status: types.StatusRunning, StepCount: 7, Trace: []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 17}}, {TokenUsage: types.TokenUsage{TotalTokens: 17}}, {TokenUsage: types.TokenUsage{TotalTokens: 17}}}}
			b := *a
			b.Status = types.StatusPaused
			b.Trace = []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 99}}}
			if err := writer.(TaskCreationStore).CreateTask(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				for i := 0; i < 100; i++ {
					task := a
					if i%2 == 0 {
						task = &b
					}
					if err := writer.SaveFullTask(t.Context(), task); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			var readErr error
			for i := 0; i < 100; i++ {
				s, err := reader.GetTaskStreamSnapshot(t.Context(), a.ID)
				if err != nil {
					readErr = err
					break
				}
				if !((s.Status == types.StatusRunning && s.TraceCount == 3 && s.TokenUsage.TotalTokens == 51) || (s.Status == types.StatusPaused && s.TraceCount == 1 && s.TokenUsage.TotalTokens == 99)) {
					readErr = fmt.Errorf("mixed state/Trace snapshot: %+v", s)
					break
				}
			}
			writeErr := <-done
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
		})
	}
}
