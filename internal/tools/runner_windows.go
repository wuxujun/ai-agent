//go:build windows

package tools

import "os/exec"

// CommandContext's default cancellation terminates the command process on
// Windows. Unix needs additional process-group handling for child processes.
func configureCommandCancellation(_ *exec.Cmd) {}
