package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deterministically cancels between phases without timing sleeps or production
// hooks. Err is only called synchronously by the patch under test.
type patchCancelContext struct {
	context.Context
	remaining int
	cause     error
}

func (c *patchCancelContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return c.cause
	}
	return nil
}

func TestSearchReplaceCancellationDoesNotCommit(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, check := range []int{1, 5, 8} {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			if err := os.WriteFile(path, []byte("old value"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx := &patchCancelContext{Context: context.Background(), remaining: check, cause: cause}
			err := applySearchReplaceBlocks(ctx, dir, path, []patchBlock{{search: "old", replace: "new"}, {search: "value", replace: "text"}})
			if !errors.Is(err, cause) {
				t.Fatalf("check %d: %v", check, err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != "old value" {
				t.Fatalf("cancel committed: %q", got)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatal("temporary patch file leaked")
			}
		}
	}
}

func TestPatchFileByteBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, size := range []int{maxPatchFileBytes, maxPatchFileBytes + 1} {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0600); err != nil {
			t.Fatal(err)
		}
		data, err := readWorkspaceFileSecurely(t.Context(), root, "file.txt")
		if size == maxPatchFileBytes {
			if err != nil || len(data) != size {
				t.Fatalf("boundary: %d %v", len(data), err)
			}
		} else if err == nil {
			t.Fatal("oversized input accepted")
		}
	}
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	err = applySearchReplaceBlocks(t.Context(), dir, path, []patchBlock{{search: "old", replace: strings.Repeat("x", maxPatchFileBytes+1)}})
	if err == nil {
		t.Fatal("oversized output accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old" {
		t.Fatal("oversized output modified target")
	}
	if err := writeWorkspaceFileAtomically(t.Context(), root, "file.txt", make([]byte, maxPatchFileBytes+1)); err == nil {
		t.Fatal("oversized atomic write accepted")
	}
}

func TestPatchInputAndBlockBounds(t *testing.T) {
	if err := (&ApplyPatchTool{}).Validate(map[string]any{"path": "file.txt", "patch": strings.Repeat("x", maxPatchBytes+1)}); err == nil {
		t.Fatal("oversized patch accepted")
	}
	block := "<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\n"
	if _, err := parseSearchReplacePatch(strings.Repeat(block, maxPatchBlocks+1)); err == nil {
		t.Fatal("excess blocks accepted")
	}
	if _, err := applyUnifiedHunks(t.Context(), []byte(strings.Repeat("\n", maxPatchLines+1)), "file.txt", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n-\n+x\n"); err == nil {
		t.Fatal("excess source lines accepted")
	}
}
