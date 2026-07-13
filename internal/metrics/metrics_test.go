package metrics

import (
	"context"
	"errors"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestCollectorTracksLLMUsageCostAndReliability(t *testing.T) {
	collector := NewCollector()
	collector.ObserveLLMCallContext(context.Background(), llmcore.CallEvent{
		Scene:            "writer",
		Provider:         "litellm",
		Model:            "agent-writer",
		Usage:            types.TokenUsage{PromptTokens: 100, CompletionTokens: 25, TotalTokens: 125},
		Err:              errors.New("failed"),
		EstimatedCostUSD: 0.001,
	})
	for _, kind := range []llmcore.ReliabilityEventKind{
		llmcore.ReliabilityCircuitOpened,
		llmcore.ReliabilityCircuitRejected,
		llmcore.ReliabilityRetryBudgetExhausted,
		llmcore.ReliabilityTaskBudgetRejected,
		llmcore.ReliabilityFallbackSucceeded,
		llmcore.ReliabilityFallbackFailed,
	} {
		collector.ObserveLLMReliability(context.Background(), llmcore.ReliabilityEvent{Kind: kind, Scene: "writer"})
	}

	snapshot := collector.Snapshot()
	if snapshot.LLMSceneCalls != 1 || snapshot.LLMSceneErrors != 1 {
		t.Fatalf("call counters = %+v", snapshot)
	}
	if snapshot.LLMPromptTokens != 100 || snapshot.LLMCompletionTokens != 25 || snapshot.LLMTotalTokens != 125 {
		t.Fatalf("token counters = %+v", snapshot)
	}
	if snapshot.LLMEstimatedCostUSD != 0.001 {
		t.Fatalf("estimated cost = %f, want 0.001", snapshot.LLMEstimatedCostUSD)
	}
	if snapshot.LLMCircuitOpened != 1 || snapshot.LLMCircuitRejected != 1 || snapshot.LLMRetryBudgetExhausted != 1 || snapshot.LLMTaskBudgetRejected != 1 || snapshot.LLMFallbackSucceeded != 1 || snapshot.LLMFallbackFailed != 1 {
		t.Fatalf("reliability counters = %+v", snapshot)
	}
}
