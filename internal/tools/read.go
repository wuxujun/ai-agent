package tools

import (
	"os"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/policy"
)

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
