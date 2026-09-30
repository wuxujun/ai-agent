package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
