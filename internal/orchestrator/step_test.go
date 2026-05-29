package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStepNextExecutesStaticSteps(t *testing.T) {
	// Create a temporary workspace directory for testing tools
	tempDir, err := os.MkdirTemp("", "step-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy text file
	filePath := filepath.Join(tempDir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("some needle content here"), 0644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	engine := &Engine{
		Mode: ModeStep,
	}

	task := &types.Task{
		ID:         "task-step-1",
		Goal:       "find needle",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
		Workspace:  tempDir,
	}

	// Step 0: stepFindTextFiles
	if err := engine.Next(context.Background(), task); err != nil {
		if strings.Contains(err.Error(), "dependency 'find' is not installed") || strings.Contains(err.Error(), "dependency 'rg' is not installed") {
			t.Skip("Skipping test due to missing system dependency")
			return
		}
		t.Fatalf("Step 0 failed: %v", err)
	}
	if task.StepCount != 1 {
		t.Errorf("expected step count 1, got %d", task.StepCount)
	}
	if len(task.Trace) != 1 {
		t.Errorf("expected 1 trace, got %d", len(task.Trace))
	}
	if task.Trace[0].Action != "find_files" {
		t.Errorf("expected action 'find_files', got %s", task.Trace[0].Action)
	}

	// Step 1: stepSearchKeyword
	if err := engine.Next(context.Background(), task); err != nil {
		if strings.Contains(err.Error(), "dependency 'rg' is not installed") {
			t.Skip("Skipping test due to missing ripgrep dependency")
			return
		}
		t.Fatalf("Step 1 failed: %v", err)
	}
	if task.StepCount != 2 {
		t.Errorf("expected step count 2, got %d", task.StepCount)
	}
	if len(task.Trace) != 2 {
		t.Errorf("expected 2 traces, got %d", len(task.Trace))
	}
	if task.Trace[1].Action != "search_text" {
		t.Errorf("expected action 'search_text', got %s", task.Trace[1].Action)
	}

	// Step 2: stepReadBestFile
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatalf("Step 2 failed: %v", err)
	}
	if task.StepCount != 3 {
		t.Errorf("expected step count 3, got %d", task.StepCount)
	}
	if len(task.Trace) != 3 {
		t.Errorf("expected 3 traces, got %d", len(task.Trace))
	}
	if task.Trace[2].Action != "read_file" {
		t.Errorf("expected action 'read_file', got %s", task.Trace[2].Action)
	}
	if task.Status != types.StatusCompleted {
		t.Errorf("expected task completed, got status %s", task.Status)
	}
}
