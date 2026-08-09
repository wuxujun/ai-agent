package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
)

type fakeMCPManager struct {
	closed      bool
	hadDeadline bool
	err         error
}

func (f *fakeMCPManager) Close(ctx context.Context) error {
	f.closed = true
	_, f.hadDeadline = ctx.Deadline()
	return f.err
}

func TestBuildMCPRuntimeOwnsManager(t *testing.T) {
	manager := &fakeMCPManager{}
	registrar := func(context.Context, []config.MCPServerConfig, *tools.Registry) (mcpManagerCloser, tools.MCPRegistrationSummary, error) {
		return manager, tools.MCPRegistrationSummary{}, nil
	}
	runtime, err := buildMCPRuntimeWithRegistrar(t.Context(), &config.Config{}, tools.NewRegistry(), registrar)
	if err != nil {
		t.Fatal(err)
	}
	runtime.timeout = time.Second
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if !manager.closed || !manager.hadDeadline {
		t.Fatalf("MCP manager close state: closed=%v deadline=%v", manager.closed, manager.hadDeadline)
	}
}

func TestBuildMCPRuntimeCleansPartialInitialization(t *testing.T) {
	initErr := errors.New("required server failed")
	closeErr := errors.New("partial manager close failed")
	manager := &fakeMCPManager{err: closeErr}
	registrar := func(context.Context, []config.MCPServerConfig, *tools.Registry) (mcpManagerCloser, tools.MCPRegistrationSummary, error) {
		return manager, tools.MCPRegistrationSummary{}, initErr
	}
	runtime, err := buildMCPRuntimeWithRegistrar(t.Context(), &config.Config{}, tools.NewRegistry(), registrar)
	if runtime != nil {
		t.Fatalf("runtime = %#v, want nil after initialization failure", runtime)
	}
	if !manager.closed {
		t.Fatal("partially initialized MCP manager was not closed")
	}
	for _, want := range []error{initErr, closeErr} {
		if !errors.Is(err, want) {
			t.Fatalf("buildMCPRuntimeWithRegistrar() error = %v, missing %v", err, want)
		}
	}
}

func TestNilMCPRuntimeClose(t *testing.T) {
	var runtime *mcpRuntime
	if err := runtime.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
}
