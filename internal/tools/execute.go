package tools

import (
	"strings"
)

// ExecuteCode executes a command (e.g. python3, bash, go) inside the workspace
// with the provided space-separated arguments.
func ExecuteCode(workspace string, command string, argsStr string) (string, error) {
	var args []string
	if strings.TrimSpace(argsStr) != "" {
		args = strings.Fields(argsStr)
	}

	return RunCommand(workspace, command, args...)
}
