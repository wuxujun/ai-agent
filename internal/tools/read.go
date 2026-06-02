package tools

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file in the workspace"
}

func (t *ReadFileTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	path, _ := params["path"].(string)
	content, err := ReadFile(workspace, path)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Query:       path,
		Observation: "read file content: " + content,
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
