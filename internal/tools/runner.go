package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/policy/commandenv"
	"github.com/wuxujun/ai-agent/internal/policy/containersandbox"
	"go.opentelemetry.io/otel/trace"
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

	var out boundedCommandOutput
	var stderr boundedCommandOutput
	cfg := config.Get()
	sandbox, extra := cfg.Tool.Sandbox, append([]string(nil), cfg.Tool.EnvironmentAllowlist...)
	var err error
	if validationErr := sandbox.Validate(); validationErr != nil {
		return "", validationErr
	}
	if sandbox.Docker() {
		if workspaceErr := policy.ValidateWorkspace(dir); workspaceErr != nil {
			return "", workspaceErr
		}
		err = containersandbox.Run(ctx, sandbox, dir, name, args, extra, &out, &stderr)
		if errors.Is(err, containersandbox.ErrCleanup) {
			fields := []any{"task_id", logger.TaskID(ctx), "error", err}
			if span := trace.SpanContextFromContext(ctx); span.IsValid() {
				fields = append(fields, "trace_id", span.TraceID().String())
			}
			logger.Component("tools.runner").ErrorContext(ctx, "sandbox cleanup unconfirmed", fields...)
		}
	} else {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		environment, envErr := commandenv.Build(os.Environ(), dir, extra)
		if envErr != nil {
			return "", envErr
		}
		cmd.Env = environment
		configureCommandCancellation(cmd)
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err = cmd.Run()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, containersandbox.ErrCleanup) {
			if stderr.Len() > 0 {
				return stderr.String(), ctxErr
			}
			return out.String(), ctxErr
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("dependency '%s' is not installed on this system; please install it (e.g., 'brew install %s' or 'apt install %s')", name, name, name)
		}
		if code, exited := commandExitCode(err); exited {
			if code == 1 && name == "rg" {
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

// Continue draining pipes after the bound so subprocesses can exit normally.
// stdout and stderr have separate writers, preserving existing failure output
// selection while bounding retained memory on success, failure and cancellation.
const maxCommandOutputBytes = 1 << 20
const commandOutputTruncated = "\n[output truncated]\n"

type boundedCommandOutput struct {
	data      []byte
	truncated bool
}

func (b *boundedCommandOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, maxCommandOutputBytes-len(b.data))
	b.data = append(b.data, p[:keep]...)
	if keep < n {
		b.truncated = true
	}
	return n, nil
}
func (b *boundedCommandOutput) Len() int { return len(b.data) }
func (b *boundedCommandOutput) String() string {
	if !b.truncated {
		return string(b.data)
	}
	return string(b.data[:min(len(b.data), maxCommandOutputBytes-len(commandOutputTruncated))]) + commandOutputTruncated
}

// Cleanup and cancellation failures must never be converted into ordinary
// tool observations, even when joined with a command exit error.
func commandExitCode(err error) (int, bool) {
	if errors.Is(err, containersandbox.ErrCleanup) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false
	}
	var exited interface{ ExitCode() int }
	if errors.As(err, &exited) {
		return exited.ExitCode(), true
	}
	return 0, false
}
