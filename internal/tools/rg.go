package tools

import (
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

func SearchWithRG(workspace string, query string, glob string) ([]types.Evidence, string, error) {
	args := []string{"-n", "--no-heading", "--color", "never"}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	args = append(args, query, ".")

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
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		results = append(results, types.Evidence{
			Path:  parts[0],
			Lines: []string{parts[1] + ": " + parts[2]},
			Query: query,
		})
		if len(results) >= 8 {
			break
		}
	}

	return results, out, nil
}
