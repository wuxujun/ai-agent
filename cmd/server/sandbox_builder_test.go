package main

import (
	"context"
	"github.com/wuxujun/ai-agent/internal/config"
	"testing"
)

func TestExecutionSandboxStartup(t *testing.T) {
	if validateExecutionSandbox(context.Background(), nil) == nil {
		t.Fatal("missing configuration accepted")
	}
	cfg := &config.Config{}
	if err := validateExecutionSandbox(context.Background(), cfg); err != nil {
		t.Fatalf("local mode required Docker: %v", err)
	}
	cfg.Tool.Sandbox.Mode = "unknown"
	if validateExecutionSandbox(context.Background(), cfg) == nil {
		t.Fatal("unknown backend accepted")
	}
	cfg.Tool.Sandbox.Mode = "docker"
	if validateExecutionSandbox(context.Background(), cfg) == nil {
		t.Fatal("invalid Docker configuration accepted")
	}
}
