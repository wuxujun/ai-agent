package tools

import (
	"context"
	"fmt"
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
		if err := applySearchReplaceBlocks(fullPath, blocks); err != nil {
			return nil, fmt.Errorf("failed to apply SEARCH/REPLACE blocks: %w", err)
		}
		return &ToolResult{
			Query:       path,
			Observation: fmt.Sprintf("Successfully applied %d SEARCH/REPLACE block(s) to %s", len(blocks), path),
		}, nil
	}

	// 2. Fall back to Unified Diff via git apply or patch command
	output, err := applyUnifiedDiff(ctx, workspace, path, patch)
	if err != nil {
		return nil, fmt.Errorf("failed to apply unified diff: %w", err)
	}

	return &ToolResult{
		Query:       path,
		Observation: fmt.Sprintf("Successfully applied unified diff to %s.\nCommand Output:\n%s", path, output),
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

func applySearchReplaceBlocks(filePath string, blocks []patchBlock) error {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read target file: %w", err)
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

	return os.WriteFile(filePath, []byte(content), 0644)
}

func applyUnifiedDiff(ctx context.Context, workspace, relativePath, patchContent string) (string, error) {
	tempFile, err := os.CreateTemp(workspace, "patch_*.diff")
	if err != nil {
		return "", fmt.Errorf("failed to create temp patch file: %w", err)
	}
	tempName := tempFile.Name()
	defer os.Remove(tempName)

	if _, err := tempFile.WriteString(patchContent); err != nil {
		tempFile.Close()
		return "", fmt.Errorf("failed to write temp patch file: %w", err)
	}
	tempFile.Close()

	relPatchName := filepath.Base(tempName)

	// Try git apply first
	output, err := RunCommand(ctx, workspace, "git", "apply", relPatchName)
	if err == nil {
		return output, nil
	}

	// Fallback to patch utility
	output2, err2 := RunCommand(ctx, workspace, "patch", "-p1", "-i", relPatchName)
	if err2 != nil {
		return "", fmt.Errorf("git apply failed (%v, output: %s) AND patch command failed (%v, output: %s)", err, output, err2, output2)
	}
	return output2, nil
}

func init() {
	Register(&ApplyPatchTool{})
}
