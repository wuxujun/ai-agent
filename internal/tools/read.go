package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, _ := params["path"].(string)
	content, err := ReadFile(workspace, path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
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

	// Resolve all path components beneath one opened root, even if a directory
	// or symlink changes after the policy check. Nonblocking open prevents a
	// raced-in FIFO from hanging before the descriptor can be inspected.
	relative, err := filepath.Rel(workspace, full)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Stat(relative)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read_file target must be a regular file")
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|readFileNonblock, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read_file target must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(file, 4001))
	if err != nil {
		return "", err
	}
	content := strings.ToValidUTF8(string(b), "\uFFFD")
	if len(content) > 4000 {
		cut := 4000
		for cut > 0 && !utf8.ValidString(content[:cut]) {
			cut--
		}
		content = content[:cut]
	}
	return content, nil
}
