package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy"
)

// toolTimeout returns the command execution timeout from the configuration.
func toolTimeout() time.Duration {
	cfg := config.Get()
	if cfg.Tool.TimeoutSeconds > 0 {
		return time.Duration(cfg.Tool.TimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}

func RunCommand(ctx context.Context, dir string, name string, args ...string) (string, error) {
	if err := policy.ValidateCommand(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if stderr.Len() > 0 {
				return stderr.String(), ctxErr
			}
			return out.String(), ctxErr
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("dependency '%s' is not installed on this system; please install it (e.g., 'brew install %s' or 'apt install %s')", name, name, name)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 && name == "rg" {
				return out.String(), nil
			}
		}
		if stderr.Len() > 0 {
			return stderr.String(), err
		}
		return out.String(), err
	}
	return out.String(), nil
}
