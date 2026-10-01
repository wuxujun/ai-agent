package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/policy"
)

func TestApplyPatchRejectsSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink ancestor semantics differ on Windows")
	}
	workspace, err := os.MkdirTemp(".", "apply-patch-workspace-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	outside, err := os.MkdirTemp(".", "apply-patch-outside-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	if err := os.Symlink(filepath.Join("..", outside), filepath.Join(workspace, "dir")); err != nil {
		t.Fatal(err)
	}
	outsideTarget := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideTarget, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	patch := strings.Join([]string{"<<<<<<< SEARCH", "old", "=======", "new", ">>>>>>> REPLACE"}, "\n")
	_, err = (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "dir/target.txt", "patch": patch})
	if err == nil {
		t.Fatal("apply_patch succeeded through a symlinked ancestor")
	}
	got, readErr := os.ReadFile(outsideTarget)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old" {
		t.Fatalf("outside target was modified: %q", got)
	}
}

func TestApplyPatchRejectsAncestorSwappedAfterValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(workspace, "dir")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "target.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideTarget := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := policy.ValidateReadPath(workspace, filepath.Join(parent, "target.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, filepath.Join(workspace, "old-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceFileSecurely(root, "dir/target.txt"); err == nil {
		t.Fatal("read followed the swapped ancestor outside the workspace")
	}
	if err := writeWorkspaceFileAtomically(root, "dir/target.txt", []byte("modified")); err == nil {
		t.Fatal("write followed the swapped ancestor outside the workspace")
	}
	outsideTemp := filepath.Join(outside, ".patch.tmp.known")
	if err := os.WriteFile(outsideTemp, []byte("outside temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("dir/.patch.tmp.known", "dir/target.txt"); err == nil {
		t.Fatal("rename followed the swapped ancestor outside the workspace")
	}
	got, err := os.ReadFile(outsideTarget)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("outside target was modified: %q", got)
	}
}

func TestApplyPatchPreservesTargetPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not portable to Windows")
	}
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := applySearchReplaceBlocks(workspace, target, []patchBlock{{search: "old", replace: "new"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("target permissions = %o, want 750", got)
	}
}
