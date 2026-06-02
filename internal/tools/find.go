package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type FindFilesTool struct{}

func (t *FindFilesTool) Name() string {
	return "find_files"
}

func (t *FindFilesTool) Description() string {
	return "Find files matching a pattern in the workspace"
}

func (t *FindFilesTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("find_files policy violation: %w", err)
	}
	pattern, _ := params["pattern"].(string)
	files, err := FindFiles(workspace, pattern)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Query:       pattern,
		Observation: fmt.Sprintf("found %d candidate files", len(files)),
	}, nil
}

func init() {
	Register(&FindFilesTool{})
}

func FindFiles(workspace string, pattern string) ([]string, error) {
	out, err := RunCommand(workspace, "find", ".", "-type", "f", "-name", pattern)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var files []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		files = append(files, line)
		if len(files) >= 20 {
			break
		}
	}
	return files, nil
}
