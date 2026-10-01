package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type ApplyPatchTool struct{}

func (t *ApplyPatchTool) Name() string {
	return "apply_patch"
}

func (t *ApplyPatchTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelHigh
}

func (t *ApplyPatchTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{} // High risk tools do not auto-retry
}

func (t *ApplyPatchTool) Description() string {
	return "Apply a patch to a file in the workspace. Supports both SEARCH/REPLACE blocks and Unified Diff formats."
}

func (t *ApplyPatchTool) Parameters() map[string]any {
	return map[string]any{
		"path":  map[string]any{"type": "string", "description": "Workspace-relative path of the file to patch"},
		"patch": map[string]any{"type": "string", "description": "Patch content (either SEARCH/REPLACE blocks or Unified Diff)"},
	}
}

func (t *ApplyPatchTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("apply_patch requires non-empty path")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return fmt.Errorf("invalid file path")
	}

	patch, _ := params["patch"].(string)
	if strings.TrimSpace(patch) == "" {
		return fmt.Errorf("apply_patch requires non-empty patch content")
	}

	return nil
}

func (t *ApplyPatchTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	patch, _ := params["patch"].(string)

	fullPath := filepath.Join(workspace, path)
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("apply_patch workspace policy violation: %w", err)
	}

	// Validate read/write path of the target file
	if err := policy.ValidateReadPath(workspace, fullPath); err != nil {
		return nil, fmt.Errorf("apply_patch path violation: %w", err)
	}

	// 1. Try parsing as SEARCH/REPLACE blocks
	blocks, parseErr := parseSearchReplacePatch(patch)
	if parseErr == nil && len(blocks) > 0 {
		if err := applySearchReplaceBlocks(workspace, fullPath, blocks); err != nil {
			return nil, fmt.Errorf("failed to apply SEARCH/REPLACE blocks: %w", err)
		}
		return &ToolResult{
			Query:       path,
			Observation: fmt.Sprintf("Successfully applied %d SEARCH/REPLACE block(s) to %s", len(blocks), path),
		}, nil
	}

	// 2. Apply a single-file Unified Diff through the same confined I/O path.
	if err := applyUnifiedDiff(ctx, workspace, path, patch); err != nil {
		return nil, fmt.Errorf("failed to apply unified diff: %w", err)
	}

	return &ToolResult{
		Query:       path,
		Observation: fmt.Sprintf("Successfully applied unified diff to %s", path),
	}, nil
}

type patchBlock struct {
	search  string
	replace string
}

func parseSearchReplacePatch(patch string) ([]patchBlock, error) {
	var blocks []patchBlock
	lines := strings.Split(patch, "\n")

	inSearch := false
	inReplace := false
	var searchLines []string
	var replaceLines []string

	for lineNum, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "<<<<<<< SEARCH") {
			if inSearch || inReplace {
				return nil, fmt.Errorf("line %d: unexpected <<<<<<< SEARCH inside another block", lineNum+1)
			}
			inSearch = true
			searchLines = nil
			continue
		}
		if trimmed == "=======" {
			if !inSearch {
				return nil, fmt.Errorf("line %d: unexpected ======= outside SEARCH block", lineNum+1)
			}
			inSearch = false
			inReplace = true
			replaceLines = nil
			continue
		}
		if strings.HasPrefix(trimmed, ">>>>>>> REPLACE") {
			if !inReplace {
				return nil, fmt.Errorf("line %d: unexpected >>>>>>> REPLACE outside REPLACE block", lineNum+1)
			}
			inReplace = false
			blocks = append(blocks, patchBlock{
				search:  strings.Join(searchLines, "\n"),
				replace: strings.Join(replaceLines, "\n"),
			})
			continue
		}

		if inSearch {
			searchLines = append(searchLines, line)
		} else if inReplace {
			replaceLines = append(replaceLines, line)
		}
	}

	if inSearch || inReplace {
		return nil, fmt.Errorf("incomplete SEARCH/REPLACE block at end of patch")
	}

	return blocks, nil
}

func applySearchReplaceBlocks(workspace, filePath string, blocks []patchBlock) error {
	if err := policy.ValidateReadPath(workspace, filePath); err != nil {
		return err
	}
	if err := policy.ValidateWritePath(workspace, filePath); err != nil {
		return err
	}
	relativePath, err := filepath.Rel(workspace, filePath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("failed to open workspace root: %w", err)
	}
	defer root.Close()

	b, err := readWorkspaceFileSecurely(root, relativePath)
	if err != nil {
		return err
	}
	content := string(b)

	for i, block := range blocks {
		// Verify unique occurrence
		count := strings.Count(content, block.search)
		if count == 0 {
			return fmt.Errorf("block %d: SEARCH block not found in the file", i+1)
		}
		if count > 1 {
			return fmt.Errorf("block %d: SEARCH block found multiple times (%d) in the file; please provide a more unique SEARCH block", i+1, count)
		}

		content = strings.Replace(content, block.search, block.replace, 1)
	}

	return writeWorkspaceFileAtomically(root, relativePath, []byte(content))
}

func readWorkspaceFileSecurely(root *os.Root, relativePath string) ([]byte, error) {
	info, err := root.Lstat(relativePath)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect target file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("patch target must be a regular file")
	}
	// Root confines every path component to the opened workspace even if an
	// ancestor changes between Lstat and OpenFile.
	fRead, err := root.OpenFile(relativePath, os.O_RDONLY|policy.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open file safely for reading: %w", err)
	}
	defer fRead.Close()

	b, err := io.ReadAll(fRead)
	if err != nil {
		return nil, fmt.Errorf("failed to read target file safely: %w", err)
	}
	return b, nil
}

func writeWorkspaceFileAtomically(root *os.Root, relativePath string, content []byte) error {
	info, err := root.Lstat(relativePath)
	if err != nil {
		return fmt.Errorf("failed to inspect target file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("patch target must be a regular file")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("failed to generate temp file name: %w", err)
	}
	tmpName := filepath.Join(filepath.Dir(relativePath), ".patch.tmp."+hex.EncodeToString(random))
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create safe temp file: %w", err)
	}
	defer root.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp patch content: %w", err)
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to preserve target permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp patch file: %w", err)
	}
	if err := root.Rename(tmpName, relativePath); err != nil {
		return fmt.Errorf("failed to atomically replace target file: %w", err)
	}
	return nil
}

func init() {
	Register(&ApplyPatchTool{})
}
