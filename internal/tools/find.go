package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type FindFilesTool struct{}

func (t *FindFilesTool) Name() string {
	return "find_files"
}

func (t *FindFilesTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *FindFilesTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, Backoff: time.Second}
}

func (t *FindFilesTool) Description() string {
	return "Find files matching a pattern in the workspace"
}

func (t *FindFilesTool) Parameters() map[string]any {
	return map[string]any{
		"pattern": map[string]any{"type": "string", "description": "Filename glob pattern, e.g. *.go"},
	}
}

func (t *FindFilesTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("find_files policy violation: %w", err)
	}
	pattern, _ := params["pattern"].(string)
	files, err := FindFiles(ctx, workspace, pattern)
	if err != nil {
		return nil, err
	}
	var evidence []types.Evidence
	for _, f := range files {
		evidence = append(evidence, types.Evidence{
			Path:  f,
			Lines: []string{"<file found>"},
			Query: pattern,
		})
	}
	return &ToolResult{
		Query:       pattern,
		Observation: fmt.Sprintf("found %d candidate files matching %q", len(files), pattern),
		Evidence:    evidence,
	}, nil
}

func init() {
	Register(&FindFilesTool{})
}

func FindFiles(ctx context.Context, workspace string, pattern string) ([]string, error) {
	out, err := RunCommand(ctx, workspace, "find", ".", "-type", "f", "-name", pattern)
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
