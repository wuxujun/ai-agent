package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestBuildApprovalRequestForExecuteCodeRedactsAndPreviewsCommand(t *testing.T) {
	engine := &Engine{}
	task := &types.Task{ID: "task-approval", Workspace: "workspace/demo"}

	req := engine.BuildApprovalRequest(task, "execute_code", map[string]any{
		"command":    "git",
		"args":       "status --short",
		"api_key":    "sk-secret",
		"irrelevant": "value",
	})

	if req.TaskID != task.ID {
		t.Fatalf("task id = %q, want %q", req.TaskID, task.ID)
	}
	if req.RiskLevel != types.RiskLevelHigh {
		t.Fatalf("risk = %q, want high", req.RiskLevel)
	}
	if req.Parameters["api_key"] != "[redacted]" {
		t.Fatalf("api_key was not redacted: %#v", req.Parameters["api_key"])
	}
	if !strings.Contains(req.Preview, "git status --short") {
		t.Fatalf("command preview missing command: %q", req.Preview)
	}
	if !strings.Contains(req.Preview, task.Workspace) {
		t.Fatalf("command preview missing workspace: %q", req.Preview)
	}
}

func TestBuildApprovalRequestForWriteFileIncludesDiffPreview(t *testing.T) {
	tmpDir, err := os.MkdirTemp(".", "approval_preview")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "note.txt"), []byte("old\n"), 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	req := (&Engine{}).BuildApprovalRequest(&types.Task{ID: "task-write", Workspace: tmpDir}, "write_file", map[string]any{
		"path":    "note.txt",
		"content": "new\n",
	})

	if req.Parameters["content"] != "<4 chars>" {
		t.Fatalf("content parameter should be summarized, got %#v", req.Parameters["content"])
	}
	for _, want := range []string{"--- current", "+++ proposed", "- old", "+ new"} {
		if !strings.Contains(req.Preview, want) {
			t.Fatalf("write preview missing %q:\n%s", want, req.Preview)
		}
	}
}

func TestBuildApprovalRequestForWriteFileDoesNotReadOutsideWorkspace(t *testing.T) {
	root, err := os.MkdirTemp(".", "approval_outside")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	defer os.RemoveAll(root)

	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("do-not-leak\n"), 0644); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}

	req := (&Engine{}).BuildApprovalRequest(&types.Task{ID: "task-write", Workspace: workspace}, "write_file", map[string]any{
		"path":    "../secret.txt",
		"content": "replacement\n",
	})

	if strings.Contains(req.Preview, "do-not-leak") {
		t.Fatalf("preview leaked outside-workspace file content:\n%s", req.Preview)
	}
	if !strings.Contains(req.Preview, "Policy warning") {
		t.Fatalf("preview should include policy warning for outside path:\n%s", req.Preview)
	}
}
