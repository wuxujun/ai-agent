package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestCollectorTracksRetrievalPhasesAndFallbacks(t *testing.T) {
	collector := NewCollector()
	collector.ObserveRetrieval(context.Background(), "bm25", 2*time.Millisecond, 8, false, nil)
	collector.ObserveRetrieval(context.Background(), "pgvector", 3*time.Millisecond, 6, true, errors.New("vector unavailable"))
	collector.ObserveRetrieval(context.Background(), "rrf", time.Millisecond, 3, false, nil)
	collector.IncRetrievalFallback(context.Background())

	snapshot := collector.Snapshot()
	if snapshot.RetrievalCalls != 3 || snapshot.RetrievalFailures != 1 || snapshot.RetrievalFallbacks != 1 || snapshot.RetrievalSlowPhases != 1 {
		t.Fatalf("retrieval counters = %+v", snapshot)
	}
	if snapshot.RetrievalBM25Calls != 1 || snapshot.RetrievalPGVectorCalls != 1 || snapshot.RetrievalRRFCalls != 1 {
		t.Fatalf("retrieval stage counters = %+v", snapshot)
	}
	if snapshot.RetrievalBM25Items != 8 || snapshot.RetrievalBM25LatencySum != 2*time.Millisecond || snapshot.RetrievalBM25Failures != 0 {
		t.Fatalf("BM25 metrics = %+v", snapshot)
	}
	if snapshot.RetrievalPGVectorItems != 6 || snapshot.RetrievalPGVectorLatencySum != 3*time.Millisecond || snapshot.RetrievalPGVectorFailures != 1 {
		t.Fatalf("pgvector metrics = %+v", snapshot)
	}
	if snapshot.RetrievalRRFItems != 3 || snapshot.RetrievalRRFLatencySum != time.Millisecond || snapshot.RetrievalRRFFailures != 0 {
		t.Fatalf("RRF metrics = %+v", snapshot)
	}
	if snapshot.RetrievalItems != 17 || snapshot.RetrievalLatencySum != 6*time.Millisecond {
		t.Fatalf("retrieval totals = %+v", snapshot)
	}
	if snapshot.RetrievalAverageLatencyMS != 2 || snapshot.RetrievalBM25AverageLatencyMS != 2 || snapshot.RetrievalPGVectorAverageLatencyMS != 3 || snapshot.RetrievalRRFAverageLatencyMS != 1 {
		t.Fatalf("retrieval averages = %+v", snapshot)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"retrieval_average_latency_ms", "retrieval_bm25_average_latency_ms", "retrieval_pgvector_average_latency_ms", "retrieval_rrf_average_latency_ms", "retrieval_slow_phases"} {
		if !strings.Contains(string(payload), `"`+field+`"`) {
			t.Fatalf("metrics JSON missing %q: %s", field, payload)
		}
	}
}

func TestCollectorTracksDurableApprovalLifecycle(t *testing.T) {
	collector := NewCollector()
	ctx := context.Background()
	for _, event := range []string{"created", "approved", "rejected", "consumed", "expired", "conflict", "recovery_success", "recovery_failure", "unknown-value"} {
		collector.ObserveDurableApproval(ctx, event)
	}
	snapshot := collector.Snapshot()
	if snapshot.DurableApprovalsCreated != 1 || snapshot.DurableApprovalsApproved != 1 || snapshot.DurableApprovalsRejected != 1 || snapshot.DurableApprovalsConsumed != 1 ||
		snapshot.DurableApprovalsExpired != 1 || snapshot.DurableApprovalConflicts != 1 || snapshot.DurableApprovalRecoverySuccesses != 1 || snapshot.DurableApprovalRecoveryFailures != 1 {
		t.Fatalf("durable approval counters = %+v", snapshot)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"durable_approvals_created", "durable_approvals_consumed", "durable_approval_conflicts", "durable_approval_recovery_successes", "durable_approval_recovery_failures"} {
		if !strings.Contains(string(payload), `"`+field+`"`) {
			t.Fatalf("metrics JSON missing %q: %s", field, payload)
		}
	}
}

func TestCollectorTracksApprovalCleanup(t *testing.T) {
	collector := NewCollector()
	collector.ObserveApprovalCleanup(context.Background(), 3, nil)
	collector.ObserveApprovalCleanup(context.Background(), 0, errors.New("cleanup failed"))
	snapshot := collector.Snapshot()
	if snapshot.DurableApprovalsCleaned != 3 || snapshot.DurableApprovalCleanupFailures != 1 {
		t.Fatalf("cleanup metrics = %+v", snapshot)
	}
}

func TestCollectorTracksMultiAgentRuntimeRollout(t *testing.T) {
	collector := NewCollector()
	collector.ObserveMultiAgentRuntime("dag", "success", 10*time.Millisecond)
	collector.ObserveMultiAgentRuntime("dag", "failure", 20*time.Millisecond)
	collector.ObserveMultiAgentRuntime("dag", "partial", 2*time.Millisecond)
	collector.ObserveMultiAgentRuntime("legacy", "canceled", 5*time.Millisecond)
	collector.ObserveMultiAgentRuntimeFallback("dag_fallback:research_replan_escalated_to_reviewed")
	collector.ObserveMultiAgentRuntimeEvent("dag", "approval_required")
	collector.ObserveMultiAgentRuntimeEvent("dag", "replanned")
	collector.ObserveMultiAgentRuntimeEvent("dag", "observed")
	collector.ObserveMultiAgentRuntimeEvent("legacy", "replanned")
	collector.ObserveMultiAgentRuntimeEvent("legacy", "observed")
	collector.ObserveMultiAgentRuntimeEvent("dag", "unknown")
	snapshot := collector.Snapshot()
	if snapshot.MultiAgentDAGCalls != 3 || snapshot.MultiAgentDAGCompletions != 1 || snapshot.MultiAgentDAGFailures != 1 || snapshot.MultiAgentDAGFallbacks != 1 || snapshot.MultiAgentDAGLatencySum != 32*time.Millisecond {
		t.Fatalf("DAG rollout metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentLegacyCalls != 1 || snapshot.MultiAgentLegacyCompletions != 0 || snapshot.MultiAgentLegacyFailures != 1 || snapshot.MultiAgentLegacyLatencySum != 5*time.Millisecond {
		t.Fatalf("Legacy rollout metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentDAGApprovalRequired != 1 || snapshot.MultiAgentDAGReplanned != 1 || snapshot.MultiAgentDAGEventsObserved != 1 || snapshot.MultiAgentLegacyApprovalRequired != 0 || snapshot.MultiAgentLegacyReplanned != 1 || snapshot.MultiAgentLegacyEventsObserved != 1 {
		t.Fatalf("runtime event metrics = %+v", snapshot)
	}
}

func TestCollectorTracksMultiAgentTeamSelections(t *testing.T) {
	collector := NewCollector()
	collector.ObserveMultiAgentTeamSelection(t.Context(), "wiki", "created", false)
	collector.ObserveMultiAgentTeamSelection(t.Context(), "wiki", "created", true)
	collector.ObserveMultiAgentTeamSelection(t.Context(), "software", "forbidden", true)

	snapshot := collector.Snapshot()
	if snapshot.MultiAgentTeamTasksCreated != 2 || snapshot.MultiAgentTeamTasksCreatedByTeam["wiki"] != 2 {
		t.Fatalf("created team metrics = %+v", snapshot)
	}
	if snapshot.MultiAgentTeamSelectionRejections != 1 || snapshot.MultiAgentTeamDefaultUnavailable != 1 {
		t.Fatalf("rejected team metrics = %+v", snapshot)
	}
	snapshot.MultiAgentTeamTasksCreatedByTeam["wiki"] = 99
	if got := collector.Snapshot().MultiAgentTeamTasksCreatedByTeam["wiki"]; got != 2 {
		t.Fatalf("snapshot map aliases collector state: %d", got)
	}
}

func TestCollectorTracksMultiAgentTeamConfigEvents(t *testing.T) {
	collector := NewCollector()
	collector.ObserveMultiAgentTeamConfigEvent(t.Context(), "readiness_failure")
	collector.ObserveMultiAgentTeamConfigEvent(t.Context(), "reload_rejected")
	collector.ObserveMultiAgentTeamConfigEvent(t.Context(), "unknown")
	snapshot := collector.Snapshot()
	if snapshot.MultiAgentTeamReadinessFailures != 1 || snapshot.MultiAgentTeamReloadRejections != 1 {
		t.Fatalf("team config event metrics = %+v", snapshot)
	}
}
