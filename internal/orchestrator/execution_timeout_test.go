package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestPausableTimeoutExcludesApprovalWait(t *testing.T) {
	ctx, cancel := WithPausableTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	resume := PauseExecutionTimeout(ctx)
	time.Sleep(100 * time.Millisecond)
	if err := ctx.Err(); err != nil {
		t.Fatalf("paused context expired during approval wait: %v", err)
	}
	resume()
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("Err() = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("resumed execution timeout did not expire")
	}
}

func TestPausableTimeoutCancelWhilePaused(t *testing.T) {
	ctx, cancel := WithPausableTimeout(context.Background(), time.Second)
	resume := PauseExecutionTimeout(ctx)
	defer resume()
	cancel()
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatalf("Err() = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt paused timeout")
	}
}

func TestRunAllRecordsClientDisconnectCause(t *testing.T) {
	ctx, cancel := WithPausableTimeout(context.Background(), time.Second)
	defer cancel()
	CancelExecution(ctx, ErrClientDisconnected)

	engine := &Engine{}
	task := &types.Task{ID: "client-disconnected", Status: types.StatusRunning, MaxSteps: 1, ToolBudget: 1}
	err := engine.RunAll(ctx, task)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll error = %v, want context.Canceled", err)
	}
	if task.FinalAnswer != "" || task.ErrorCode != "client_disconnected" || task.ErrorMessage != "Task was canceled because the streaming client disconnected." {
		t.Fatalf("canceled task result = %+v", task)
	}
}
