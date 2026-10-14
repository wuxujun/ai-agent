package tools

import (
	"context"
	"errors"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy/containersandbox"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxExitClassification(t *testing.T) {
	exit := &containersandbox.ExitError{Code: 1}
	if code, ok := commandExitCode(exit); !ok || code != 1 {
		t.Fatal("Docker command exit contract lost")
	}
	for _, err := range []error{errors.Join(exit, containersandbox.ErrCleanup), errors.Join(exit, context.Canceled), context.DeadlineExceeded, errors.New("daemon unavailable")} {
		if _, ok := commandExitCode(err); ok {
			t.Fatal("infrastructure failure treated as command exit")
		}
	}
}
func TestDockerDoesNotFallBackToHost(t *testing.T) {
	// A valid configuration with an unavailable daemon must not run the host
	// command. Use a workspace inside cwd so it reaches the socket preflight.
	t.Setenv("TMPDIR", ".")
	t.Setenv("TMP", ".")
	t.Setenv("TEMP", ".")
	work, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, err = filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.Tool.Sandbox = containersandbox.Config{Mode: "docker", SocketPath: filepath.Join(work, "absent.sock"), Image: "sha256:" + strings.Repeat("a", 64), User: "65532:65532", CPUs: 1, MemoryMiB: 256, PidsLimit: 64, TmpfsMiB: 64}
	}))
	out, err := RunCommand(t.Context(), work, "sh", "-c", "printf host-command-ran")
	if err == nil || out != "" || !strings.Contains(err.Error(), "Unix socket") {
		t.Fatalf("Docker socket failure did not fail closed: %v", err)
	}
}
