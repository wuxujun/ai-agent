package tools

import (
	"os"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/policy"
)

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
