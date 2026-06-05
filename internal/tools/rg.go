package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type SearchTextTool struct{}

func (t *SearchTextTool) Name() string {
	return "search_text"
}

func (t *SearchTextTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *SearchTextTool) Description() string {
	return "Search for text matching a regex query in the workspace"
}

func (t *SearchTextTool) Parameters() map[string]any {
	return map[string]any{
		"query": map[string]any{"type": "string", "description": "Regex pattern to search for"},
		"glob":  map[string]any{"type": "string", "description": "Optional file glob to restrict the search, e.g. *.go"},
	}
}

func (t *SearchTextTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("search_text policy violation: %w", err)
	}
	query, _ := params["query"].(string)
	glob, _ := params["glob"].(string)
	evidence, _, err := SearchWithRG(ctx, workspace, query, glob)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Query:       query,
		Observation: fmt.Sprintf("found %d evidence items", len(evidence)),
		Evidence:    evidence,
	}, nil
}

func init() {
	Register(&SearchTextTool{})
}

func SearchWithRG(ctx context.Context, workspace string, query string, glob string) ([]types.Evidence, string, error) {
	args := []string{"-n", "--no-heading", "--color", "never"}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	args = append(args, query, ".")

	out, err := RunCommand(ctx, workspace, "rg", args...)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, "", err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	results := make([]types.Evidence, 0)

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		results = append(results, types.Evidence{
			Path:  parts[0],
			Lines: []string{parts[1] + ": " + parts[2]},
			Query: query,
		})
		if len(results) >= 8 {
			break
		}
	}

	return results, out, nil
}
