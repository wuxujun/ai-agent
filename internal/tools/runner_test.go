package tools

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRunCommandHonorsContextCancellation(t *testing.T) {
	tmpDir, err := os.MkdirTemp(".", "runner_cancel")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = RunCommand(ctx, tmpDir, "sh", "-c", "sleep 5")
	if err == nil {
		t.Fatal("expected canceled command to return an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		t.Fatalf("expected context deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("command did not stop promptly after context cancellation: %s", elapsed)
	}
}
