package tools

import (
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// GrepFiles searches for a regex pattern across all files in the workspace using ripgrep.
// It is similar to SearchWithRG but uses regex matching (-e flag) and accepts a case-insensitive option.
// Returns matched evidence items (up to 20), the raw output, and any error.
func GrepFiles(workspace, pattern, glob string, caseInsensitive bool) ([]types.Evidence, string, error) {
	args := []string{"-n", "--no-heading", "--color", "never"}
	if caseInsensitive {
		args = append(args, "-i")
	}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	// Use -e for explicit regex pattern to prevent ambiguity with file arguments
	args = append(args, "-e", pattern, ".")

	out, err := RunCommand(workspace, "rg", args...)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, "", err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	results := make([]types.Evidence, 0)

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Line format from rg: "<file>:<lineno>:<match>"
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		results = append(results, types.Evidence{
			Path:  parts[0],
			Lines: []string{parts[1] + ": " + parts[2]},
			Query: pattern,
		})
		if len(results) >= 20 {
			break
		}
	}

	return results, out, nil
}
