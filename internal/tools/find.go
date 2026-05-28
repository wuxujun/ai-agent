package tools

import "strings"

func FindFiles(workspace string, pattern string) ([]string, error) {
	out, err := RunCommand(workspace, "find", ".", "-type", "f", "-name", pattern)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var files []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		files = append(files, line)
		if len(files) >= 20 {
			break
		}
	}
	return files, nil
}
