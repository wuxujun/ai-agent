package tools

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
)

func RunCommand(dir string, name string, args ...string) (string, error) {
	if err := policy.ValidateCommand(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if stderr.Len() > 0 {
			return stderr.String(), err
		}
		return out.String(), err
	}
	return out.String(), nil
}
