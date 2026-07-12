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

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelHigh
}

func (t *WriteFileTool) Description() string {
	return "Write content to a file in the workspace"
}

func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"path":    map[string]any{"type": "string", "description": "Workspace-relative file path to write"},
		"content": map[string]any{"type": "string", "description": "Content to write to the file"},
	}
}

func (t *WriteFileTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("write_file requires non-empty path")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return fmt.Errorf("invalid write_file path")
	}
	if _, ok := params["content"].(string); !ok {
		return fmt.Errorf("write_file requires content string parameter")
	}
	return nil
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

	// Open with O_NOFOLLOW to prevent following symlinks.
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|policy.O_NOFOLLOW, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file safely: %w", err)
	}
	defer f.Close()

	if _, err := f.Write([]byte(content)); err != nil {
		return fmt.Errorf("failed to write content safely: %w", err)
	}
	return nil
}
