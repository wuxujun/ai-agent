package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExecutorWriteFileAndExecuteCode(t *testing.T) {
	ctx := context.Background()

	// 1. Create a temp directory as workspace
	tmpDir, err := os.MkdirTemp("", "executor_test_workspace")
	if err != nil {
		t.Fatalf("failed to create temp workspace: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	execInst := &executor.DefaultExecutor{}
	task := &types.Task{
		ID:        "exec-test-task",
		Workspace: tmpDir,
		Status:    types.StatusRunning,
	}

	// 2. Test write_file action
	writeDec := &planner.PlanDecision{
		Actions: []planner.ActionCall{
			{
				Action: "write_file",
				Parameters: map[string]any{
					"path":    "hello.py",
					"content": "print('Hello from executed code!')\n",
					"pattern": "",
					"query":   "",
					"glob":    "",
					"command": "",
					"args":    "",
				},
			},
		},
	}

	traces, err := execInst.Execute(ctx, task, writeDec)
	if err != nil {
		t.Fatalf("failed to execute write_file: %v", err)
	}
	if len(traces) == 0 {
		t.Fatalf("no traces returned")
	}
	trace := traces[0]

	if !strings.Contains(trace.Observation, "successfully wrote") {
		t.Errorf("expected observation to confirm write, got: %q", trace.Observation)
	}

	// Verify file was written
	data, err := os.ReadFile(filepath.Join(tmpDir, "hello.py"))
	if err != nil {
		t.Fatalf("failed to read hello.py: %v", err)
	}
	if string(data) != "print('Hello from executed code!')\n" {
		t.Errorf("unexpected file content: %q", string(data))
	}

	// 3. Test execute_code action (running python3 on hello.py)
	execDec := &planner.PlanDecision{
		Actions: []planner.ActionCall{
			{
				Action: "execute_code",
				Parameters: map[string]any{
					"command": "python3",
					"args":    "hello.py",
					"pattern": "",
					"query":   "",
					"glob":    "",
					"path":    "",
					"content": "",
				},
			},
		},
	}

	traces2, err := execInst.Execute(ctx, task, execDec)
	if err != nil {
		t.Fatalf("failed to execute execute_code: %v", err)
	}
	if len(traces2) == 0 {
		t.Fatalf("no traces returned")
	}
	trace2 := traces2[0]

	if !strings.Contains(trace2.Observation, "command executed") {
		t.Errorf("expected observation to contain command executed, got: %q", trace2.Observation)
	}
	if !strings.Contains(trace2.Observation, "Hello from executed code!") {
		t.Errorf("expected observation to contain stdout, got: %q", trace2.Observation)
	}
}
