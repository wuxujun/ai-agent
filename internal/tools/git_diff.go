package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type GitDiffTool struct{}

func (t *GitDiffTool) Name() string {
	return "git_diff"
}

func (t *GitDiffTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *GitDiffTool) Description() string {
	return "Get the git diff for the workspace or a specific file"
}

func (t *GitDiffTool) Parameters() map[string]any {
	return map[string]any{
		"path": map[string]any{"type": "string", "description": "Optional workspace-relative path to diff; empty diffs the whole workspace"},
	}
}

func (t *GitDiffTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		// Whole-workspace diff is the documented default.
		return nil
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return fmt.Errorf("invalid git_diff path")
	}
	return nil
}

func (t *GitDiffTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("git_diff policy violation: %w", err)
	}

	args := []string{"diff"}
	path, _ := params["path"].(string)
	if path != "" {
		args = append(args, path)
	}

	out, err := RunCommand(ctx, workspace, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("git_diff failed: %w", err)
	}

	obs := strings.TrimSpace(out)
	if obs == "" {
		obs = "no changes"
	}
	if len(obs) > 4000 {
		obs = obs[:4000]
	}

	return &ToolResult{
		Query:       strings.Join(append([]string{"git"}, args...), " "),
		Observation: obs,
	}, nil
}

func init() {
	Register(&GitDiffTool{})
}
