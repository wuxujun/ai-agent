package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type GitDiffTool struct{}

func (t *GitDiffTool) Name() string {
	return "git_diff"
}

func (t *GitDiffTool) Description() string {
	return "Get the git diff for the workspace or a specific file"
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

	out, err := RunCommand(workspace, "git", args...)
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
