//go:build unix

package tools

import (
	"context"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy/containersandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandDockerLive(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_CONTAINER_TESTS") != "true" {
		t.Skip("real Docker acceptance not enabled")
	}
	c := containersandbox.Config{Mode: "docker", SocketPath: os.Getenv("AI_AGENT_CONTAINER_TEST_SOCKET"), Image: os.Getenv("AI_AGENT_CONTAINER_TEST_IMAGE"), User: "65532:65532", CPUs: 2, MemoryMiB: 1024, PidsLimit: 128, TmpfsMiB: 512}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Tool.Sandbox = c
		cfg.Tool.TimeoutSeconds = 120
		cfg.Tool.EnvironmentAllowlist = nil
	}))
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
	if err := os.Chmod(work, 0777); err != nil {
		t.Fatal(err)
	}
	out, err := RunCommand(t.Context(), work, "python3", "-c", "import sys;sys.stdout.write('x'*(2*1024*1024));sys.stderr.write('e'*(2*1024*1024))")
	if err != nil || len(out) > maxCommandOutputBytes || !strings.HasSuffix(out, commandOutputTruncated) {
		t.Fatalf("output limit failed: %v bytes=%d", err, len(out))
	}
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.test/failed\n\ngo 1.26.8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "main_test.go"), []byte("package failed\nimport \"testing\"\nfunc TestFailure(t *testing.T){t.Fatal(\"intentional\")}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := (&RunTestsTool{}).Execute(context.Background(), work, map[string]any{})
	if err != nil || !strings.Contains(result.Observation, "tests failed (exit code 1)") {
		t.Fatalf("failed test contract: %v result=%v", err, result)
	}
	out, err = RunCommand(t.Context(), work, "rg", "never-present", "go.mod")
	if err != nil || out != "" {
		t.Fatalf("rg no matches: %v", err)
	}
}
