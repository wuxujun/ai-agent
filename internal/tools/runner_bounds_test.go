package tools

import (
	"strings"
	"testing"
)

func TestRunCommandBoundsSuccessAndFailureOutput(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		fails         bool
	}{
		{"stdout", "printf '%2097152s' x", false},
		{"stderr", "printf '%2097152s' x >&2; exit 7", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RunCommand(t.Context(), ".", "sh", "-c", tc.command)
			if (err != nil) != tc.fails {
				t.Fatalf("exit semantics changed: %v", err)
			}
			if len(out) > 1<<20 || !strings.Contains(out, "[output truncated]") {
				t.Fatalf("unbounded output bytes=%d", len(out))
			}
		})
	}
}
