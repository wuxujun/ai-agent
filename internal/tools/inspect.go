package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
)

// FileInfo holds metadata about a file in the workspace.
type FileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	LineCount int    `json:"line_count"`
	Extension string `json:"extension"`
	IsDir     bool   `json:"is_dir"`
}

// InspectFile returns metadata about a file in the workspace without reading its full content.
// It validates that the target path is within the workspace boundary before access.
func InspectFile(workspace, relativePath string) (*FileInfo, error) {
	full := filepath.Join(workspace, relativePath)
	if err := policy.ValidateReadPath(workspace, full); err != nil {
		return nil, fmt.Errorf("inspect_file policy violation: %w", err)
	}

	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("inspect_file: %w", err)
	}

	fi := &FileInfo{
		Path:      relativePath,
		SizeBytes: info.Size(),
		Extension: strings.TrimPrefix(filepath.Ext(info.Name()), "."),
		IsDir:     info.IsDir(),
	}

	// Count lines only for regular (non-directory) files that are reasonably small
	if !info.IsDir() && info.Size() <= 1<<20 { // ≤ 1 MiB
		data, err := os.ReadFile(full)
		if err == nil {
			fi.LineCount = strings.Count(string(data), "\n")
			if len(data) > 0 && data[len(data)-1] != '\n' {
				fi.LineCount++ // count last line without trailing newline
			}
		}
	}

	return fi, nil
}
