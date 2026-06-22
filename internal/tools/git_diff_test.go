package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestGitDiffValidateRejectsArgumentInjection is the regression test for
// BUG_REPORT.md #5: a path beginning with '-' (e.g. "--output=/tmp/evil",
// "--no-index") was accepted by Validate and passed to git as a bare option,
// allowing arbitrary file writes/reads outside the sandbox. Validate must now
// reject any leading '-' alongside the existing absolute-path and ".." guards.
func TestGitDiffValidateRejectsArgumentInjection(t *testing.T) {
	tool := &GitDiffTool{}

	bad := []string{
		"--output=/tmp/evil",
		"--no-index",
		"-O/tmp/evil",
		"/etc/passwd",
		"../escape",
		"sub/../../escape",
	}
	for _, p := range bad {
		if err := tool.Validate(map[string]any{"path": p}); err == nil {
			t.Errorf("Validate(%q) = nil, want error (injection/escape vector must be rejected)", p)
		}
	}

	ok := []string{"", "main.go", "internal/tools/git_diff.go", "a/b/c.txt"}
	for _, p := range ok {
		if err := tool.Validate(map[string]any{"path": p}); err != nil {
			t.Errorf("Validate(%q) = %v, want nil (legitimate path rejected)", p, err)
		}
	}
}

// TestGitDiffExecuteDoesNotWriteViaInjection verifies the defense-in-depth "--"
// separator: even if a '-'-prefixed path reaches Execute, git must treat it as a
// pathspec, never as the --output option, so no file is written outside the
// workspace. The path is also rejected by the re-validation in Execute.
func TestGitDiffExecuteDoesNotWriteViaInjection(t *testing.T) {
	if _, err := exec_LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// On macOS, t.TempDir() and os.TempDir() resolve to /private/var/folders/…
	// which is in the policy blocked-paths list (/private/var is blocked).
	// Create temporary directories under the current working directory (the
	// internal/tools package dir) so they fall under an allowed workspace root.
	base, err := os.MkdirTemp(".", "gitdiff_test_")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	ws, err := filepath.Abs(base)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	mustGit(t, ws, "init")
	mustGit(t, ws, "config", "user.email", "t@t.t")
	mustGit(t, ws, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ws, "add", "a.txt")
	mustGit(t, ws, "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hi\nthere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the "evil" target directory in a sibling temp dir under cwd.
	evilBase, err := os.MkdirTemp(".", "gitdiff_evil_")
	if err != nil {
		t.Fatalf("MkdirTemp (evil): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(evilBase) })
	evilDir, err := filepath.Abs(evilBase)
	if err != nil {
		t.Fatalf("Abs (evil): %v", err)
	}
	evil := filepath.Join(evilDir, "EVIL")
	tool := &GitDiffTool{}

	// Execute must reject the injection path (re-validation) and, even if it
	// did not, the "--" separator guarantees no file is written.
	_, err = tool.Execute(context.Background(), ws, map[string]any{"path": "--output=" + evil})
	if err == nil {
		t.Error("Execute with injection path = nil error, want rejection")
	}
	if _, statErr := os.Stat(evil); statErr == nil {
		t.Fatalf("git_diff wrote attacker-controlled file %q — argument injection NOT prevented", evil)
	}

	// A legitimate relative path still works.
	res, err := tool.Execute(context.Background(), ws, map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatalf("Execute with valid path = %v, want nil", err)
	}
	if res == nil || res.Observation == "" {
		t.Fatalf("Execute returned empty diff for a changed file: %+v", res)
	}
}
