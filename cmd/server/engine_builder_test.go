package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestBuildEngineWiresApprovalKeyRotation(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x61}, 32)
	newKey := bytes.Repeat([]byte{0x62}, 32)
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(newKey))
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", base64.StdEncoding.EncodeToString(oldKey))
	cfg := &config.Config{}
	cfg.Orchestrator.Mode = string(orchestrator.ModeLegacy)
	cfg.LLM.Provider = "litellm"
	cfg.LLM.BaseURL = "http://llm-gateway.test"
	st := store.NewMemoryStore()
	defer st.Close()
	build, err := buildEngine(t.Context(), cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldCodec, err := approvalcrypto.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := oldCodec.Encrypt([]byte("old-checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := build.engine.ApprovalCodec.Decrypt(payload)
	if err != nil || string(opened) != "old-checkpoint" {
		t.Fatalf("rotated codec decrypt = %q, %v", opened, err)
	}
}

func TestBuildEngineRequiresAPIKeyForActiveMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Orchestrator.Mode = string(orchestrator.ModeLegacy)
	cfg.LLM.Provider = "openai"
	st := store.NewMemoryStore()
	defer st.Close()
	build, err := buildEngine(t.Context(), cfg, st, nil)
	if err == nil || build.engine != nil {
		t.Fatalf("buildEngine() = %+v, %v; want API key error", build, err)
	}
}

func TestBuildEngineWiresCoreDependencies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Orchestrator.Mode = string(orchestrator.ModeLegacy)
	cfg.LLM.Provider = "litellm"
	cfg.LLM.BaseURL = "http://llm-gateway.test"
	st := store.NewMemoryStore()
	defer st.Close()

	build, err := buildEngine(t.Context(), cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if build.engine == nil || build.metrics == nil || build.runtime == nil {
		t.Fatalf("incomplete engine build: %+v", build)
	}
	eng := build.engine
	if eng.Store != st || eng.Mode != orchestrator.ModeLegacy || eng.Planner == nil || eng.Executor == nil || eng.AnswerPipeline == nil {
		t.Fatalf("core engine dependencies were not wired: %+v", eng)
	}
	if eng.Finalizer == nil || eng.SafetyGuard == nil || eng.PlanCritic == nil || eng.AnswerUncertaintyCalibrator == nil {
		t.Fatal("specialized engine dependencies were not wired")
	}
}

func TestBuildEnginePropagatesOllamaProbeFailure(t *testing.T) {
	probeErr := errors.New("ollama unavailable")
	cfg := &config.Config{}
	cfg.LLM.Provider = "ollama"
	cfg.LLM.BaseURL = "http://127.0.0.1:11434/api/chat"
	st := store.NewMemoryStore()
	defer st.Close()
	called := 0
	probe := func(context.Context, string, string) error {
		called++
		return probeErr
	}

	_, err := buildEngine(t.Context(), cfg, st, probe)
	if !errors.Is(err, probeErr) || called != 1 {
		t.Fatalf("buildEngine() error = %v, probe calls = %d", err, called)
	}
}

func TestBuildEngineWiresMultiAgentCoordinator(t *testing.T) {
	cfg := &config.Config{}
	cfg.Orchestrator.Mode = string(orchestrator.ModeMultiAgent)
	cfg.LLM.Provider = "litellm"
	cfg.LLM.BaseURL = "http://llm-gateway.test"
	st := store.NewMemoryStore()
	defer st.Close()

	build, err := buildEngine(t.Context(), cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if build.engine.Coordinator == nil || build.engine.Coordinator.Verifier == nil || build.engine.Coordinator.PersistTask == nil {
		t.Fatal("multi-agent coordinator dependencies were not wired")
	}
}
