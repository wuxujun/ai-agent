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

// TestValidateWorkspace_SymlinkEscape verifies that a workspace path that is
// itself a symlink (or traverses a symlink) is rejected, even if the symlink
// points to a directory that would otherwise be within the allowed zone.
func TestValidateWorkspace_SymlinkEscape(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Create a real target directory INSIDE cwd so it passes the cwd check.
	realDir, err := os.MkdirTemp(cwd, "real_ws_*")
	if err != nil {
		t.Fatalf("MkdirTemp real: %v", err)
	}
	defer os.RemoveAll(realDir)

	// Create a symlink INSIDE cwd pointing to the real dir.
	linkDir := filepath.Join(cwd, "symlink_ws_test_link")
	os.Remove(linkDir) // clean up any leftover
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("cannot create symlink (may need elevated permissions): %v", err)
	}
	defer os.Remove(linkDir)

	// The symlink path itself must be rejected — even though it points to a
	// valid directory inside cwd, allowing symlinked workspaces would let an
	// attacker swap the target later to point outside the sandbox.
	if err := policy.ValidateWorkspace(linkDir); err == nil {
		t.Error("expected error for workspace that is itself a symlink; got nil")
	}
}

// TestValidateWorkspace_BlockedPaths verifies that known sensitive system
// directories are unconditionally rejected.
func TestValidateWorkspace_BlockedPaths(t *testing.T) {
	blockedPaths := []string{
		"/etc",
		"/etc/passwd",
		"/proc",
		"/proc/1",
		"/sys",
		"/dev",
		"/root",
	}
	for _, p := range blockedPaths {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue // path does not exist on this OS; skip
		}
		if err := policy.ValidateWorkspace(p); err == nil {
			t.Errorf("expected error for blocked path %q, got nil", p)
		}
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

// TestValidateReadPath_DeepSymlinkChain verifies that a chained symlink attack
// (symlink → symlink → outside) is also caught.
//
// Layout:
//
//	tmpDir/workspace/link1  → tmpDir/intermediate/link2
//	tmpDir/intermediate/link2 → tmpDir/outside
//	tmpDir/outside/secret.txt (sensitive file)
func TestValidateReadPath_DeepSymlinkChain(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	intermediate := filepath.Join(tmpDir, "intermediate")
	outsideDir := filepath.Join(tmpDir, "outside")

	for _, d := range []string{workspace, intermediate, outsideDir} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// intermediate/link2 → outsideDir
	link2 := filepath.Join(intermediate, "link2")
	if err := os.Symlink(outsideDir, link2); err != nil {
		t.Fatalf("symlink link2: %v", err)
	}

	// workspace/link1 → intermediate (link2 is inside intermediate)
	link1 := filepath.Join(workspace, "link1")
	if err := os.Symlink(intermediate, link1); err != nil {
		t.Fatalf("symlink link1: %v", err)
	}

	// Chained access: workspace/link1/link2/secret.txt
	// Real path resolves to: outsideDir/secret.txt  (outside workspace)
	chainedPath := filepath.Join(link1, "link2", "secret.txt")
	if err := policy.ValidateReadPath(workspace, chainedPath); err == nil {
		t.Error("expected error for file accessed via chained symlinks escaping workspace; got nil")
	}
}
