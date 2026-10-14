package main

import (
	"context"
	"errors"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy/containersandbox"
)

func validateExecutionSandbox(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("execution sandbox configuration is missing")
	}
	if err := cfg.Tool.Sandbox.Validate(); err != nil {
		return err
	}
	if !cfg.Tool.Sandbox.Docker() {
		return nil
	}
	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return containersandbox.Preflight(startupCtx, cfg.Tool.Sandbox)
}
