package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type ExecuteCodeTool struct{}

func (t *ExecuteCodeTool) Name() string {
	return "execute_code"
}

func (t *ExecuteCodeTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelHigh
}

func (t *ExecuteCodeTool) Description() string {
	return "Execute a command in the workspace"
}

func (t *ExecuteCodeTool) Parameters() map[string]any {
	return map[string]any{
		"command": map[string]any{"type": "string", "description": "Executable to run; must be on the policy allowlist (e.g. python3, go, bash)"},
		"args":    map[string]any{"type": "string", "description": "Space-separated arguments passed to the command"},
	}
}

func (t *ExecuteCodeTool) Validate(params map[string]any) error {
	command, _ := params["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("execute_code requires non-empty command")
	}
	if _, ok := params["args"].(string); !ok {
		return fmt.Errorf("execute_code requires args string parameter")
	}
	return nil
}

func (t *ExecuteCodeTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("execute_code policy violation: %w", err)
	}
	command, _ := params["command"].(string)
	args, _ := params["args"].(string)
	output, err := ExecuteCode(ctx, workspace, command, args)
	if err != nil {
		return nil, fmt.Errorf("execute_code error: %w. Output: %s", err, output)
	}
	obs := output
	if len(obs) > 4000 {
		obs = obs[:4000]
	}
	return &ToolResult{
		Query:       command + " " + args,
		Observation: "command executed. Output:\n" + obs,
	}, nil
}

func init() {
	Register(&ExecuteCodeTool{})
}

// ExecuteCode executes a command (e.g. python3, bash, go) inside the workspace
// with the provided space-separated arguments.
func ExecuteCode(ctx context.Context, workspace string, command string, argsStr string) (string, error) {
	var args []string
	if strings.TrimSpace(argsStr) != "" {
		args = strings.Fields(argsStr)
	}

	return RunCommand(ctx, workspace, command, args...)
}
