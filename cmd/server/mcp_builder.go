package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
)

type mcpManagerCloser interface {
	Close(context.Context) error
}

type mcpRegistrar func(context.Context, []config.MCPServerConfig, *tools.Registry) (mcpManagerCloser, tools.MCPRegistrationSummary, error)

type mcpRuntime struct {
	manager mcpManagerCloser
	timeout time.Duration
}

func (r *mcpRuntime) Close() error {
	if r == nil || r.manager == nil {
		return nil
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.manager.Close(ctx)
}

func registerMCPTools(ctx context.Context, servers []config.MCPServerConfig, registry *tools.Registry) (mcpManagerCloser, tools.MCPRegistrationSummary, error) {
	return tools.RegisterMCPTools(ctx, servers, registry)
}

func buildMCPRuntime(ctx context.Context, cfg *config.Config, registry *tools.Registry) (*mcpRuntime, error) {
	return buildMCPRuntimeWithRegistrar(ctx, cfg, registry, registerMCPTools)
}

func buildMCPRuntimeWithRegistrar(ctx context.Context, cfg *config.Config, registry *tools.Registry, register mcpRegistrar) (*mcpRuntime, error) {
	manager, summary, err := register(ctx, cfg.MCP.Servers, registry)
	runtime := &mcpRuntime{manager: manager, timeout: 5 * time.Second}
	if err != nil {
		initErr := fmt.Errorf("initialize required MCP services: %w", err)
		if closeErr := runtime.Close(); closeErr != nil {
			return nil, errors.Join(initErr, fmt.Errorf("cleanup partially initialized MCP services: %w", closeErr))
		}
		return nil, initErr
	}
	for serverName, failure := range summary.Failures {
		slog.Warn("optional MCP service unavailable", "server", serverName, "error", failure)
	}
	if summary.ServersConfigured > 0 {
		slog.Info("MCP services initialized",
			"configured", summary.ServersConfigured,
			"ready", summary.ServersReady,
			"tools", summary.ToolsRegistered,
			"failed", len(summary.Failures),
		)
	}
	return runtime, nil
}
