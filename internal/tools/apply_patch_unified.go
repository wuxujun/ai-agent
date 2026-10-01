package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
)

var unifiedHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

type patchLine struct {
	text    string
	newline bool
}

// applyUnifiedDiff supports a single existing file. Applying hunks in memory
// lets the same os.Root operations protect both accepted patch formats.
func applyUnifiedDiff(ctx context.Context, workspace, relativePath, patchContent string) error {
	fullPath := filepath.Join(workspace, relativePath)
	if err := policy.ValidateReadPath(workspace, fullPath); err != nil {
		return err
	}
	if err := policy.ValidateWritePath(workspace, fullPath); err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("failed to open workspace root: %w", err)
	}
	defer root.Close()
	content, err := readWorkspaceFileSecurely(root, relativePath)
	if err != nil {
		return err
	}
	updated, err := applyUnifiedHunks(ctx, content, filepath.ToSlash(relativePath), patchContent)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeWorkspaceFileAtomically(root, relativePath, updated); err != nil {
		return err
	}
	return nil
}

func applyUnifiedHunks(ctx context.Context, content []byte, target, patch string) ([]byte, error) {
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	oldHeader, newHeader := false, false
	var result []patchLine
	original := splitPatchLines(string(content))
	oldPos, newPos, hunks := 0, 0, 0
	for i := 0; i < len(lines); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.HasPrefix(line, "--- ") {
			if oldHeader || hunks > 0 || !unifiedPathMatches(strings.TrimPrefix(line, "--- "), target) {
				return nil, fmt.Errorf("unified diff must modify only %s", target)
			}
			oldHeader = true
			i++
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			if !oldHeader || newHeader || !unifiedPathMatches(strings.TrimPrefix(line, "+++ "), target) {
				return nil, fmt.Errorf("unified diff must modify only %s", target)
			}
			newHeader = true
			i++
			continue
		}
		if strings.HasPrefix(line, "diff --git ") && hunks > 0 {
			return nil, fmt.Errorf("unified diff contains more than one file")
		}
		match := unifiedHunkHeader.FindStringSubmatch(line)
		if match == nil {
			if hunks > 0 {
				return nil, fmt.Errorf("unexpected content after unified diff hunk: %q", line)
			}
			i++ // git metadata before the first hunk
			continue
		}
		if !oldHeader || !newHeader {
			return nil, fmt.Errorf("unified diff is missing file headers")
		}
		oldStart, _ := strconv.Atoi(match[1])
		oldCount := hunkCount(match[2])
		newStart, _ := strconv.Atoi(match[3])
		newCount := hunkCount(match[4])
		start := oldStart - 1
		if oldCount == 0 {
			start = oldStart
		}
		newStartIndex := newStart - 1
		if newCount == 0 {
			newStartIndex = newStart
		}
		if start < oldPos || start > len(original) || newStartIndex != newPos+start-oldPos {
			return nil, fmt.Errorf("unified diff hunk has invalid line numbers")
		}
		result = append(result, original[oldPos:start]...)
		newPos += start - oldPos
		oldPos = start
		i++
		consumed, produced := 0, 0
		lastKind := byte(0)
		lastOld := -1
		for i < len(lines) {
			line = lines[i]
			if strings.TrimSuffix(line, "\r") == `\ No newline at end of file` {
				if lastKind == 0 {
					return nil, fmt.Errorf("unexpected no-newline marker")
				}
				if lastKind != '+' {
					if lastOld < 0 || original[lastOld].newline {
						return nil, fmt.Errorf("no-newline marker does not match source")
					}
				}
				if lastKind != '-' {
					result[len(result)-1].newline = false
				}
				lastKind = 0
				i++
				continue
			}
			if consumed == oldCount && produced == newCount {
				break
			}
			if line == "" || (line[0] != ' ' && line[0] != '-' && line[0] != '+') {
				return nil, fmt.Errorf("invalid unified diff hunk line: %q", line)
			}
			kind, value := line[0], line[1:]
			lastOld = -1
			switch kind {
			case ' ', '-':
				if consumed >= oldCount || oldPos >= len(original) || original[oldPos].text != value {
					return nil, fmt.Errorf("unified diff does not match source at line %d", oldPos+1)
				}
				lastOld = oldPos
				if kind == ' ' {
					if produced >= newCount {
						return nil, fmt.Errorf("unified diff hunk exceeds new line count")
					}
					result = append(result, original[oldPos])
					produced++
				}
				oldPos++
				consumed++
			case '+':
				if produced >= newCount {
					return nil, fmt.Errorf("unified diff hunk exceeds new line count")
				}
				result = append(result, patchLine{text: value, newline: true})
				produced++
			}
			lastKind = kind
			i++
		}
		if consumed != oldCount || produced != newCount {
			return nil, fmt.Errorf("incomplete unified diff hunk")
		}
		newPos += produced
		hunks++
	}
	if hunks == 0 {
		return nil, fmt.Errorf("unified diff contains no hunks")
	}
	result = append(result, original[oldPos:]...)
	var out strings.Builder
	for i, line := range result {
		if i < len(result)-1 && !line.newline {
			return nil, fmt.Errorf("missing newline before end of patched file")
		}
		out.WriteString(line.text)
		if line.newline {
			out.WriteByte('\n')
		}
	}
	return []byte(out.String()), nil
}

func unifiedPathMatches(header, target string) bool {
	name := strings.SplitN(header, "\t", 2)[0]
	return name == target || name == "a/"+target || name == "b/"+target
}

func hunkCount(raw string) int {
	if raw == "" {
		return 1
	}
	n, _ := strconv.Atoi(raw)
	return n
}

func splitPatchLines(content string) []patchLine {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, "\n")
	lines := make([]patchLine, 0, len(parts))
	for i, part := range parts {
		if i == len(parts)-1 && part == "" {
			break
		}
		lines = append(lines, patchLine{text: part, newline: i < len(parts)-1})
	}
	return lines
}
