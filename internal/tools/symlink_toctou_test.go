package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/policy"
)

// TestWriteFile_SymlinkBlocked verifies that WriteFile fails to open/write
// when the target path is a symlink (on Unix-like systems where policy.O_NOFOLLOW is active).
func TestWriteFile_SymlinkBlocked(t *testing.T) {
	if policy.O_NOFOLLOW == 0 {
		t.Skip("O_NOFOLLOW is not active on this platform (e.g. Windows); skipping symlink follow prevention test")
	}

	workspace := t.TempDir()

	// 1. Create a real file target
	realFile := filepath.Join(workspace, "real.txt")
	if err := os.WriteFile(realFile, []byte("original content"), 0644); err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	// 2. Create a symlink pointing to the real file
	linkFile := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(realFile, linkFile); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// 3. Try to use WriteFile on the link path. It must fail because O_NOFOLLOW is set.
	err := WriteFile(workspace, "link.txt", "hijacked content")
	if err == nil {
		t.Fatal("expected WriteFile to fail on symlink target, but it succeeded")
	}

	// Verify that the original content was NOT overwritten
	content, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original content" {
		t.Errorf("content was overwritten despite symlink check: %q", string(content))
	}
}

// TestApplyPatch_SymlinkBlocked verifies that applySearchReplaceBlocks fails
// when the target file is a symlink.
func TestApplyPatch_SymlinkBlocked(t *testing.T) {
	if policy.O_NOFOLLOW == 0 {
		t.Skip("O_NOFOLLOW is not active on this platform; skipping symlink follow prevention test")
	}

	workspace := t.TempDir()

	// 1. Create a real file target
	realFile := filepath.Join(workspace, "real.txt")
	if err := os.WriteFile(realFile, []byte("line A\nline B\nline C"), 0644); err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	// 2. Create a symlink pointing to the real file
	linkFile := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(realFile, linkFile); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	blocks := []patchBlock{
		{
			search:  "line B",
			replace: "line X",
		},
	}

	// 3. Try to use applySearchReplaceBlocks on the link path. It must fail.
	err := applySearchReplaceBlocks(workspace, linkFile, blocks)
	if err == nil {
		t.Fatal("expected applySearchReplaceBlocks to fail on symlink target, but it succeeded")
	}

	// Verify that the original content was NOT overwritten
	content, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "line A\nline B\nline C" {
		t.Errorf("content was overwritten: %q", string(content))
	}
}
