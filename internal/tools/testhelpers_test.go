package tools

import (
	"os"
	"os/exec"
	"testing"
)

// exec_LookPath is a thin alias for exec.LookPath so test files can reference
// it without importing "os/exec" directly.
var exec_LookPath = exec.LookPath

// mustGit runs a git sub-command inside dir and fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+t.TempDir(), // isolate from the real ~/.gitconfig
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
