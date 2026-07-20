package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestCollectorTracksMultiAgentLifecycle(t *testing.T) {
	collector := NewCollector()
	collector.ObserveMultiAgentRoute("adaptive", "planner_researcher_writer", "budget_fallback:llm_calls:complexity:high")
	collector.ObserveMultiAgentRoute("adaptive", "planner_critic_executor_verifier", "adaptive_replan:high_risk_action:write_file")
	collector.ObserveMultiAgentPhase("planner", "success", 10*time.Millisecond)
	collector.ObserveMultiAgentPhase("verifier", "error", 5*time.Millisecond)
	collector.ObserveMultiAgentCriticReview("approved")
	collector.ObserveMultiAgentCriticReview("rejected")
	collector.ObserveMultiAgentCriticReview("invalid")
	collector.IncMultiAgentCriticReplan()
	collector.ObserveMultiAgentVerifierCheckpoint("persisted")
	collector.ObserveMultiAgentVerifierResume("success")
	collector.ObserveMultiAgentVerifierResume("retryable_error")
	collector.ObserveMultiAgentConfigChange("require_match", "blocked")
	collector.ObserveMultiAgentConfigChange("use_latest", "migrated")

	snapshot := collector.Snapshot()
	if snapshot.MultiAgentRoutes != 2 || snapshot.MultiAgentBudgetFallbacks != 1 || snapshot.MultiAgentEscalations != 1 {
		t.Fatalf("route metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentPhaseCalls != 2 || snapshot.MultiAgentPhaseFailures != 1 || snapshot.MultiAgentPhaseLatencySum != 15*time.Millisecond {
		t.Fatalf("phase metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentCriticApprovals != 1 || snapshot.MultiAgentCriticRejections != 1 || snapshot.MultiAgentCriticErrors != 1 || snapshot.MultiAgentCriticReplans != 1 {
		t.Fatalf("critic metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentCheckpoints != 1 || snapshot.MultiAgentResumeAttempts != 2 || snapshot.MultiAgentResumeSuccesses != 1 || snapshot.MultiAgentResumeFailures != 1 {
		t.Fatalf("verifier metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentConfigChanges != 2 || snapshot.MultiAgentConfigBlocks != 1 || snapshot.MultiAgentConfigMigrations != 1 {
		t.Fatalf("config metrics = %+v", snapshot)
	}
}
