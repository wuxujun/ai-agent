package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestApplyPatchUnifiedDiffConfinedToRequestedFile(t *testing.T) {
	workspace := patchWorkspace(t)
	target := filepath.Join(workspace, "target.txt")
	other := filepath.Join(workspace, "other.txt")
	if err := os.WriteFile(target, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/target.txt b/target.txt\n--- a/target.txt\n+++ b/target.txt\n@@ -1,3 +1,3 @@\n first\n-second\n+changed\n third\n"
	if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "target.txt", "patch": patch}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "first\nchanged\nthird\n" {
		t.Fatalf("target = %q, error = %v", got, err)
	}
	malicious := "--- a/other.txt\n+++ b/other.txt\n@@ -1 +1 @@\n-other\n+changed\n"
	if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "target.txt", "patch": malicious}); err == nil {
		t.Fatal("unified diff modified a file other than the requested target")
	}
	if got, err := os.ReadFile(other); err != nil || string(got) != "other\n" {
		t.Fatalf("other = %q, error = %v", got, err)
	}
}

func TestApplyPatchUnifiedDiffRejectsMultipleFiles(t *testing.T) {
	workspace := patchWorkspace(t)
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-old\n+new\n--- a/other.txt\n+++ b/other.txt\n@@ -1 +1 @@\n-old\n+new\n"
	if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "target.txt", "patch": patch}); err == nil {
		t.Fatal("multi-file patch accepted")
	}
	if got, _ := os.ReadFile(target); string(got) != "old\n" {
		t.Fatalf("target modified after rejected patch: %q", got)
	}
}

func TestApplyPatchUnifiedDiffRejectsSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	workspace, outside := patchWorkspace(t), patchWorkspace(t)
	outsideTarget := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideTarget, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "dir")); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/dir/target.txt\n+++ b/dir/target.txt\n@@ -1 +1 @@\n-old\n+new\n"
	if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "dir/target.txt", "patch": patch}); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
	if got, _ := os.ReadFile(outsideTarget); string(got) != "old\n" {
		t.Fatalf("outside target modified: %q", got)
	}
}

func TestApplyPatchUnifiedDiffNoFinalNewline(t *testing.T) {
	workspace := patchWorkspace(t)
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{"--- a/target.txt", "+++ b/target.txt", "@@ -1 +1 @@", "-old", `\ No newline at end of file`, "+new", `\ No newline at end of file`, ""}, "\n")
	if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "target.txt", "patch": patch}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Fatalf("target = %q, want new without trailing newline", got)
	}
}

func TestApplyPatchUnifiedDiffMultipleHunksAndInsertion(t *testing.T) {
	tests := []struct {
		name, original, patch, want string
	}{
		{
			name:     "multiple hunks",
			original: "one\ntwo\nthree\nfour\nfive\n",
			patch:    "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-one\n+ONE\n@@ -5 +5 @@\n-five\n+FIVE\n",
			want:     "ONE\ntwo\nthree\nfour\nFIVE\n",
		},
		{
			name:     "insert into empty file",
			original: "",
			patch:    "--- a/target.txt\n+++ b/target.txt\n@@ -0,0 +1 @@\n+new\n",
			want:     "new\n",
		},
		{
			name:     "CRLF content",
			original: "old\r\nnext\r\n",
			patch:    "--- a/target.txt\n+++ b/target.txt\n@@ -1,2 +1,2 @@\n-old\r\n+new\r\n next\r\n",
			want:     "new\r\nnext\r\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace := patchWorkspace(t)
			target := filepath.Join(workspace, "target.txt")
			if err := os.WriteFile(target, []byte(tc.original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := (&ApplyPatchTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "target.txt", "patch": tc.patch}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(target); string(got) != tc.want {
				t.Fatalf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

func patchWorkspace(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", cwd)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", cwd)
		t.Setenv("TEMP", cwd)
	}
	return t.TempDir()
}
