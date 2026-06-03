package policy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/policy"
)

func TestValidateWorkspace(t *testing.T) {
	// 1. Broad workspaces
	if err := policy.ValidateWorkspace("/"); err == nil {
		t.Error("expected error for root directory workspace")
	}
	if err := policy.ValidateWorkspace("."); err == nil {
		t.Error("expected error for current directory workspace '.'")
	}

	// 2. Valid workspaces
	cwd, _ := os.Getwd()
	tmpDir, _ := os.MkdirTemp(cwd, "test_workspace_*")
	defer os.RemoveAll(tmpDir)
	if err := policy.ValidateWorkspace(tmpDir); err != nil {
		t.Errorf("unexpected error for valid temp workspace: %v", err)
	}

	// 3. Path traversals
	if err := policy.ValidateWorkspace("../outside"); err == nil {
		t.Error("expected error for workspace with '..'")
	}
}

func TestValidateCommand(t *testing.T) {
	allowed := []string{"find", "rg", "cat", "python3", "bash"}
	for _, cmd := range allowed {
		if err := policy.ValidateCommand(cmd); err != nil {
			t.Errorf("expected command %q to be allowed, got error: %v", cmd, err)
		}
	}

	forbidden := []string{"rm", "curl", "wget", "sudo", "docker"}
	for _, cmd := range forbidden {
		if err := policy.ValidateCommand(cmd); err == nil {
			t.Errorf("expected command %q to be blocked, but got no error", cmd)
		}
	}
}

func TestValidateReadPath_Traversals(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}

	// Create a safe file inside workspace
	safeFile := filepath.Join(workspace, "hello.txt")
	if err := os.WriteFile(safeFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write safe file: %v", err)
	}

	// 1. Check safe path validation
	if err := policy.ValidateReadPath(workspace, safeFile); err != nil {
		t.Errorf("unexpected error for safe file inside workspace: %v", err)
	}
	if err := policy.ValidateReadPath(workspace, workspace); err != nil {
		t.Errorf("unexpected error for workspace path itself: %v", err)
	}

	// 2. Check dot-dot escape
	escapeFile := filepath.Join(workspace, "../outside.txt")
	if err := policy.ValidateReadPath(workspace, escapeFile); err == nil {
		t.Error("expected error for path traversing outside workspace via '..'")
	}
}

func TestValidateReadPath_SymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	outsideDir := filepath.Join(tmpDir, "outside")

	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	if err := os.Mkdir(outsideDir, 0755); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}

	// Create a file outside workspace
	sensitiveFile := filepath.Join(outsideDir, "secrets.txt")
	if err := os.WriteFile(sensitiveFile, []byte("sensitive info"), 0644); err != nil {
		t.Fatalf("failed to write sensitive file: %v", err)
	}

	// Create a symlink INSIDE workspace pointing to the sensitive directory OUTSIDE
	symlinkPath := filepath.Join(workspace, "link_to_outside")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// 1. Check direct symlink access pointing outside
	if err := policy.ValidateReadPath(workspace, symlinkPath); err == nil {
		t.Error("expected error for symlink pointing to outside directory")
	}

	// 2. Check access to file inside the symlink directory
	symlinkFile := filepath.Join(symlinkPath, "secrets.txt")
	if err := policy.ValidateReadPath(workspace, symlinkFile); err == nil {
		t.Error("expected error for file accessed through escape symlink")
	}

	// 3. Check writing to non-existing file inside escaped symlink directory
	nonExistingSymlinkFile := filepath.Join(symlinkPath, "new_file.txt")
	if err := policy.ValidateWritePath(workspace, nonExistingSymlinkFile); err == nil {
		t.Error("expected error for writing through escape symlink")
	}
}
