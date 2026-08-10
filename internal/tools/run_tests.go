package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

// RunTestsTool executes a constrained Go test command. It deliberately does
// not accept a command name or arbitrary argument list.
type RunTestsTool struct{}

func (t *RunTestsTool) Name() string { return "run_tests" }

func (t *RunTestsTool) RiskLevel() types.RiskLevel { return types.RiskLevelHigh }

func (t *RunTestsTool) RetryPolicy() RetryPolicy { return RetryPolicy{} }

func (t *RunTestsTool) Description() string {
	return "Run a constrained Go test target in the workspace; requires approval because tests execute project code"
}

func (t *RunTestsTool) Parameters() map[string]any {
	return map[string]any{
		"package": map[string]any{"type": "string", "description": "Workspace-relative Go package target such as ./... or ./internal/store"},
		"run":     map[string]any{"type": "string", "description": "Optional Go test -run regular expression"},
		"race":    map[string]any{"type": "boolean", "description": "Enable the Go race detector"},
	}
}

func (t *RunTestsTool) Validate(params map[string]any) error {
	target, _ := params["package"].(string)
	if err := validateGoTestTarget(target); err != nil {
		return err
	}
	if value, ok := params["run"]; ok {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("run_tests run must be a string")
		}
	}
	if value, ok := params["race"]; ok {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("run_tests race must be a boolean")
		}
	}
	return nil
}

func validateGoTestTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if strings.ContainsAny(target, " \t\r\n") || filepath.IsAbs(target) || !strings.HasPrefix(target, "./") {
		return fmt.Errorf("run_tests package must be a workspace-relative ./ target")
	}
	base := strings.TrimSuffix(target, "/...")
	if base == "." {
		return nil
	}
	for _, segment := range strings.Split(strings.TrimPrefix(base, "./"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid run_tests package target")
		}
		for _, r := range segment {
			if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
				return fmt.Errorf("invalid run_tests package target")
			}
		}
	}
	return nil
}

func (t *RunTestsTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := t.Validate(params); err != nil {
		return nil, err
	}
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("run_tests policy violation: %w", err)
	}
	target, _ := params["package"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		target = "./..."
	}
	args := []string{"test"}
	if race, _ := params["race"].(bool); race {
		args = append(args, "-race")
	}
	if runPattern, _ := params["run"].(string); strings.TrimSpace(runPattern) != "" {
		args = append(args, "-run", strings.TrimSpace(runPattern))
	}
	args = append(args, target)

	output, err := RunCommand(ctx, workspace, "go", args...)
	query := "go " + strings.Join(args, " ")
	if err == nil {
		return &ToolResult{Query: query, Observation: "tests passed\n" + output}, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ToolResult{Query: query, Observation: fmt.Sprintf("tests failed (exit code %d)\n%s", exitErr.ExitCode(), output)}, nil
	}
	return nil, fmt.Errorf("run_tests execution failed: %w", err)
}

func init() {
	Register(&RunTestsTool{})
}
