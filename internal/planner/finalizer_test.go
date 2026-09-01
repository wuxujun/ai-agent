package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestBuildFinalizerEvidenceIncludesFetchedEvidenceContent(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Step: 2, Action: "rag_fetch", Observation: "fetched 1 rag item(s)",
		Evidence: []types.Evidence{{Path: "rag-1", Query: "学术顾问", Lines: []string{"顾问姓名：张三"}}},
	}}}
	got := buildFinalizerEvidence(task)
	for _, want := range []string{"fetched 1 rag item(s)", "rag-1", "学术顾问", "顾问姓名：张三"} {
		if !strings.Contains(got, want) {
			t.Fatalf("finalizer evidence missing %q: %s", want, got)
		}
	}
}

func TestNewLLMTaskFinalizer_FailedCallKeepsLegacyZeroUsage(t *testing.T) {
	caller := &failedFinalizerCaller{usage: types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))

	_, usage, err := NewLLMTaskFinalizer("legacy-finalizer").Finalize(ctx, &types.Task{Goal: "answer"})
	if err == nil {
		t.Fatal("expected finalizer error")
	}
	if usage != (types.TokenUsage{}) {
		t.Fatalf("legacy usage = %#v, want zero", usage)
	}
}

func TestNewFrozenLLMTaskFinalizer_FailedCallPreservesUsageAndConfig(t *testing.T) {
	wantConfig := llmcore.Config{
		Scene:                   "task_finalizer",
		Provider:                "openai",
		APIKey:                  "secret",
		Model:                   "writer-frozen",
		InputCostPerMillionUSD:  2,
		OutputCostPerMillionUSD: 4,
	}
	wantUsage := types.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}
	caller := &failedFinalizerCaller{usage: wantUsage}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))

	_, usage, err := NewFrozenLLMTaskFinalizer(wantConfig).Finalize(ctx, &types.Task{Goal: "answer"})
	if err == nil {
		t.Fatal("expected finalizer error")
	}
	if usage != wantUsage {
		t.Fatalf("frozen usage = %#v, want %#v", usage, wantUsage)
	}
	if caller.cfg != wantConfig {
		t.Fatalf("call config = %+v, want %+v", caller.cfg, wantConfig)
	}
}

type failedFinalizerCaller struct {
	cfg   llmcore.Config
	usage types.TokenUsage
}

func (c *failedFinalizerCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, _ string, _ map[string]any, _ any) (types.TokenUsage, error) {
	c.cfg = cfg
	return c.usage, errors.New("writer failed after usage")
}
