package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Description() string {
	return "Write content to a file in the workspace"
}

func (t *WriteFileTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("write_file policy violation: %w", err)
	}
	path, _ := params["path"].(string)
	content, _ := params["content"].(string)
	err := WriteFile(workspace, path, content)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Query:       path,
		Observation: fmt.Sprintf("successfully wrote %d characters to %s", len(content), path),
	}, nil
}

func init() {
	Register(&WriteFileTool{})
}

// WriteFile writes the given content to a relative path inside the workspace.
// It automatically creates any parent directories that do not exist.
func WriteFile(workspace string, relativePath string, content string) error {
	full := filepath.Join(workspace, relativePath)
	if err := policy.ValidateWritePath(workspace, full); err != nil {
		return err
	}

	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(full, []byte(content), 0644)
}
