package orchestrator

import (
	"context"
	"testing"
	"time"
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
