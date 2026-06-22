package tools

import (
	"context"
	"fmt"
	"github.com/wuxujun/ai-agent/internal/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file in the workspace"
}

func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"path": map[string]any{"type": "string", "description": "Workspace-relative file path to read"},
	}
}

func (t *ReadFileTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("read_file requires non-empty path")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return fmt.Errorf("invalid read_file path")
	}
	return nil
}

func (t *ReadFileTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	path, _ := params["path"].(string)
	content, err := ReadFile(workspace, path)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Query:       path,
		Observation: fmt.Sprintf("read %d char(s) from %q", len(content), path),
		Evidence: []types.Evidence{{
			Path:  path,
			Lines: []string{content},
			Query: path,
		}},
	}, nil
}

func init() {
	Register(&ReadFileTool{})
}

func ReadFile(workspace string, relativePath string) (string, error) {
	full := filepath.Join(workspace, relativePath)
	if err := policy.ValidateReadPath(workspace, full); err != nil {
		return "", err
	}

	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}

	if len(b) > 4000 {
		b = b[:4000]
	}
	return string(b), nil
}
