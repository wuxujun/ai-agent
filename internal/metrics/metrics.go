package metrics

import (
	"context"
	"strings"
	"sync"
	"time"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"
)

type Snapshot struct {
	PlannerCalls                          int64            `json:"planner_calls"`
	PlannerFailures                       int64            `json:"planner_failures"`
	PlannerLatencySum                     time.Duration    `json:"planner_latency_sum"`
	WriterCalls                           int64            `json:"writer_calls"`
	WriterFailures                        int64            `json:"writer_failures"`
	WriterLatencySum                      time.Duration    `json:"writer_latency_sum"`
	ExecutorCalls                         int64            `json:"executor_calls"`
	ExecutorFailures                      int64            `json:"executor_failures"`
	ExecutorLatencySum                    time.Duration    `json:"executor_latency_sum"`
	RunAllCalls                           int64            `json:"run_all_calls"`
	TasksCompleted                        int64            `json:"tasks_completed"`
	FallbackHits                          int64            `json:"fallback_hits"`
	PromptTokens                          int64            `json:"prompt_tokens"`
	CompletionTokens                      int64            `json:"completion_tokens"`
	TotalTokens                           int64            `json:"total_tokens"`
	LLMSceneCalls                         int64            `json:"llm_scene_calls"`
	LLMSceneErrors                        int64            `json:"llm_scene_errors"`
	LLMPromptTokens                       int64            `json:"llm_prompt_tokens"`
	LLMCompletionTokens                   int64            `json:"llm_completion_tokens"`
	LLMTotalTokens                        int64            `json:"llm_total_tokens"`
	LLMEstimatedCostUSD                   float64          `json:"llm_estimated_cost_usd"`
	LLMCircuitOpened                      int64            `json:"llm_circuit_opened"`
	LLMCircuitRejected                    int64            `json:"llm_circuit_rejected"`
	LLMRetryBudgetExhausted               int64            `json:"llm_retry_budget_exhausted"`
	LLMTaskBudgetRejected                 int64            `json:"llm_task_budget_rejected"`
	LLMFallbackSucceeded                  int64            `json:"llm_fallback_succeeded"`
	LLMFallbackFailed                     int64            `json:"llm_fallback_failed"`
	AnswerPipelineRuns                    int64            `json:"answer_pipeline_runs"`
	AnswerPipelineStages                  int64            `json:"answer_pipeline_stages"`
	AnswerPipelineWarnings                int64            `json:"answer_pipeline_warnings"`
	MultiAgentRoutes                      int64            `json:"multiagent_routes"`
	MultiAgentBudgetFallbacks             int64            `json:"multiagent_budget_fallbacks"`
	MultiAgentEscalations                 int64            `json:"multiagent_escalations"`
	MultiAgentPhaseCalls                  int64            `json:"multiagent_phase_calls"`
	MultiAgentPhaseFailures               int64            `json:"multiagent_phase_failures"`
	MultiAgentPhaseLatencySum             time.Duration    `json:"multiagent_phase_latency_sum"`
	MultiAgentCriticApprovals             int64            `json:"multiagent_critic_approvals"`
	MultiAgentCriticRejections            int64            `json:"multiagent_critic_rejections"`
	MultiAgentCriticErrors                int64            `json:"multiagent_critic_errors"`
	MultiAgentCriticReplans               int64            `json:"multiagent_critic_replans"`
	MultiAgentCheckpoints                 int64            `json:"multiagent_verifier_checkpoints"`
	MultiAgentResumeAttempts              int64            `json:"multiagent_verifier_resume_attempts"`
	MultiAgentResumeSuccesses             int64            `json:"multiagent_verifier_resume_successes"`
	MultiAgentResumeFailures              int64            `json:"multiagent_verifier_resume_failures"`
	MultiAgentConfigChanges               int64            `json:"multiagent_config_changes"`
	MultiAgentConfigBlocks                int64            `json:"multiagent_config_blocks"`
	MultiAgentConfigMigrations            int64            `json:"multiagent_config_migrations"`
	MultiAgentTeamTasksCreated            int64            `json:"multiagent_team_tasks_created"`
	MultiAgentTeamTasksCreatedByTeam      map[string]int64 `json:"multiagent_team_tasks_created_by_team"`
	MultiAgentTeamTasksCreatedBySource    map[string]int64 `json:"multiagent_team_tasks_created_by_source"`
	MultiAgentTeamSelectionRejections     int64            `json:"multiagent_team_selection_rejections"`
	MultiAgentTeamRejectionsBySource      map[string]int64 `json:"multiagent_team_rejections_by_source"`
	MultiAgentTeamDrainingRejections      int64            `json:"multiagent_team_draining_rejections"`
	MultiAgentTeamRetiredRejections       int64            `json:"multiagent_team_retired_rejections"`
	MultiAgentTeamDefaultUnavailable      int64            `json:"multiagent_team_default_unavailable"`
	MultiAgentTeamReadinessFailures       int64            `json:"multiagent_team_readiness_failures"`
	MultiAgentTeamReloadRejections        int64            `json:"multiagent_team_reload_rejections"`
	MultiAgentTeamLifecycleChanges        int64            `json:"multiagent_team_lifecycle_changes"`
	MultiAgentTeamLifecycleConflicts      int64            `json:"multiagent_team_lifecycle_conflicts"`
	MultiAgentTeamDefaultProtections      int64            `json:"multiagent_team_default_protections"`
	MultiAgentTeamAuditArchives           int64            `json:"multiagent_team_audit_archives"`
	MultiAgentTeamAuditArchiveConflicts   int64            `json:"multiagent_team_audit_archive_conflicts"`
	MultiAgentTeamAuditCapacityRejections int64            `json:"multiagent_team_audit_capacity_rejections"`
	MultiAgentTeamAuditIntegrityFailures  int64            `json:"multiagent_team_audit_integrity_failures"`
	MultiAgentDAGCalls                    int64            `json:"multiagent_dag_calls"`
	MultiAgentDAGCompletions              int64            `json:"multiagent_dag_completions"`
	MultiAgentDAGFailures                 int64            `json:"multiagent_dag_failures"`
	MultiAgentDAGFallbacks                int64            `json:"multiagent_dag_fallbacks"`
	MultiAgentDAGApprovalRequired         int64            `json:"multiagent_dag_approval_required"`
	MultiAgentDAGReplanned                int64            `json:"multiagent_dag_replanned"`
	MultiAgentDAGEventsObserved           int64            `json:"multiagent_dag_events_observed"`
	MultiAgentDAGLatencySum               time.Duration    `json:"multiagent_dag_latency_sum"`
	MultiAgentLegacyCalls                 int64            `json:"multiagent_legacy_calls"`
	MultiAgentLegacyCompletions           int64            `json:"multiagent_legacy_completions"`
	MultiAgentLegacyFailures              int64            `json:"multiagent_legacy_failures"`
	MultiAgentLegacyApprovalRequired      int64            `json:"multiagent_legacy_approval_required"`
	MultiAgentLegacyReplanned             int64            `json:"multiagent_legacy_replanned"`
	MultiAgentLegacyEventsObserved        int64            `json:"multiagent_legacy_events_observed"`
	MultiAgentLegacyLatencySum            time.Duration    `json:"multiagent_legacy_latency_sum"`
	RetrievalCalls                        int64            `json:"retrieval_calls"`
	RetrievalFailures                     int64            `json:"retrieval_failures"`
	RetrievalFallbacks                    int64            `json:"retrieval_fallbacks"`
	RetrievalSlowPhases                   int64            `json:"retrieval_slow_phases"`
	RetrievalItems                        int64            `json:"retrieval_items"`
	RetrievalLatencySum                   time.Duration    `json:"retrieval_latency_sum"`
	RetrievalAverageLatencyMS             float64          `json:"retrieval_average_latency_ms"`
	RetrievalBM25Calls                    int64            `json:"retrieval_bm25_calls"`
	RetrievalBM25Failures                 int64            `json:"retrieval_bm25_failures"`
	RetrievalBM25Items                    int64            `json:"retrieval_bm25_items"`
	RetrievalBM25LatencySum               time.Duration    `json:"retrieval_bm25_latency_sum"`
	RetrievalBM25AverageLatencyMS         float64          `json:"retrieval_bm25_average_latency_ms"`
	RetrievalPGVectorCalls                int64            `json:"retrieval_pgvector_calls"`
	RetrievalPGVectorFailures             int64            `json:"retrieval_pgvector_failures"`
	RetrievalPGVectorItems                int64            `json:"retrieval_pgvector_items"`
	RetrievalPGVectorLatencySum           time.Duration    `json:"retrieval_pgvector_latency_sum"`
	RetrievalPGVectorAverageLatencyMS     float64          `json:"retrieval_pgvector_average_latency_ms"`
	RetrievalRRFCalls                     int64            `json:"retrieval_rrf_calls"`
	RetrievalRRFFailures                  int64            `json:"retrieval_rrf_failures"`
	RetrievalRRFItems                     int64            `json:"retrieval_rrf_items"`
	RetrievalRRFLatencySum                time.Duration    `json:"retrieval_rrf_latency_sum"`
	RetrievalRRFAverageLatencyMS          float64          `json:"retrieval_rrf_average_latency_ms"`
	DurableApprovalsCreated               int64            `json:"durable_approvals_created"`
	DurableApprovalsApproved              int64            `json:"durable_approvals_approved"`
	DurableApprovalsRejected              int64            `json:"durable_approvals_rejected"`
	DurableApprovalsConsumed              int64            `json:"durable_approvals_consumed"`
	DurableApprovalsExpired               int64            `json:"durable_approvals_expired"`
	DurableApprovalConflicts              int64            `json:"durable_approval_conflicts"`
	DurableApprovalRecoverySuccesses      int64            `json:"durable_approval_recovery_successes"`
	DurableApprovalRecoveryFailures       int64            `json:"durable_approval_recovery_failures"`
	DurableApprovalsCleaned               int64            `json:"durable_approvals_cleaned"`
	DurableApprovalCleanupFailures        int64            `json:"durable_approval_cleanup_failures"`
}

type Collector struct {
	mu sync.Mutex
	s  Snapshot

	plannerCalls     api.Int64Counter
	plannerFailures  api.Int64Counter
	plannerLatencyMs api.Float64Histogram

	writerCalls     api.Int64Counter
	writerFailures  api.Int64Counter
	writerLatencyMs api.Float64Histogram

	executorCalls     api.Int64Counter
	executorFailures  api.Int64Counter
	executorLatencyMs api.Float64Histogram

	runAllCalls    api.Int64Counter
	tasksCompleted api.Int64Counter
	fallbackHits   api.Int64Counter

	promptTokens               api.Int64Counter
	completionTokens           api.Int64Counter
	totalTokens                api.Int64Counter
	llmSceneCalls              api.Int64Counter
	llmSceneErrors             api.Int64Counter
	llmSceneLatency            api.Float64Histogram
	llmScenePromptTokens       api.Int64Counter
	llmSceneCompletionTokens   api.Int64Counter
	llmSceneTotalTokens        api.Int64Counter
	llmSceneEstimatedCost      api.Float64Counter
	llmCircuitOpened           api.Int64Counter
	llmCircuitRejected         api.Int64Counter
	llmRetryBudgetExhausted    api.Int64Counter
	llmTaskBudgetRejected      api.Int64Counter
	llmFallbackSucceeded       api.Int64Counter
	llmFallbackFailed          api.Int64Counter
	answerPipelineRuns         api.Int64Counter
	answerPipelineStages       api.Int64Counter
	answerPipelineDuration     api.Float64Histogram
	answerPipelineTokens       api.Int64Counter
	answerPipelineWarnings     api.Int64Counter
	answerPipelineConfidence   api.Int64Counter
	multiAgentRoutes           api.Int64Counter
	multiAgentPhases           api.Int64Counter
	multiAgentPhaseLatency     api.Float64Histogram
	multiAgentCriticReviews    api.Int64Counter
	multiAgentCriticReplans    api.Int64Counter
	multiAgentCheckpoints      api.Int64Counter
	multiAgentResumes          api.Int64Counter
	multiAgentConfigChanges    api.Int64Counter
	multiAgentTeamSelections   api.Int64Counter
	multiAgentTeamConfigEvents api.Int64Counter
	multiAgentRuntimeCalls     api.Int64Counter
	multiAgentRuntimeLatency   api.Float64Histogram
	multiAgentRuntimeFallbacks api.Int64Counter
	multiAgentRuntimeEvents    api.Int64Counter
	retrievalCalls             api.Int64Counter
	retrievalFailures          api.Int64Counter
	retrievalFallbacks         api.Int64Counter
	retrievalSlowPhases        api.Int64Counter
	retrievalItems             api.Int64Counter
	retrievalLatency           api.Float64Histogram
	approvalEvents             api.Int64Counter
}

func NewCollector() *Collector {
	meter := otel.Meter("ai-agent")

	plannerCalls, _ := meter.Int64Counter("agent.planner.calls")
	plannerFailures, _ := meter.Int64Counter("agent.planner.failures")
	plannerLatencyMs, _ := meter.Float64Histogram("agent.planner.latency_ms")

	writerCalls, _ := meter.Int64Counter("agent.writer.calls")
	writerFailures, _ := meter.Int64Counter("agent.writer.failures")
	writerLatencyMs, _ := meter.Float64Histogram("agent.writer.latency_ms")

	executorCalls, _ := meter.Int64Counter("agent.executor.calls")
	executorFailures, _ := meter.Int64Counter("agent.executor.failures")
	executorLatencyMs, _ := meter.Float64Histogram("agent.executor.latency_ms")

	runAllCalls, _ := meter.Int64Counter("agent.run_all.calls")
	tasksCompleted, _ := meter.Int64Counter("agent.tasks.completed")
	fallbackHits, _ := meter.Int64Counter("agent.planner.fallback_hits")

	promptTokens, _ := meter.Int64Counter("agent.tokens.prompt")
	completionTokens, _ := meter.Int64Counter("agent.tokens.completion")
	totalTokens, _ := meter.Int64Counter("agent.tokens.total")
	llmSceneCalls, _ := meter.Int64Counter("agent.llm.scene.calls")
	llmSceneErrors, _ := meter.Int64Counter("agent.llm.scene.errors")
	llmSceneLatency, _ := meter.Float64Histogram("agent.llm.scene.latency_ms")
	llmScenePromptTokens, _ := meter.Int64Counter("agent.llm.scene.prompt_tokens")
	llmSceneCompletionTokens, _ := meter.Int64Counter("agent.llm.scene.completion_tokens")
	llmSceneTotalTokens, _ := meter.Int64Counter("agent.llm.scene.total_tokens")
	llmSceneEstimatedCost, _ := meter.Float64Counter("agent.llm.scene.estimated_cost_usd")
	llmCircuitOpened, _ := meter.Int64Counter("agent.llm.circuit.opened")
	llmCircuitRejected, _ := meter.Int64Counter("agent.llm.circuit.rejected")
	llmRetryBudgetExhausted, _ := meter.Int64Counter("agent.llm.retry.budget_exhausted")
	llmTaskBudgetRejected, _ := meter.Int64Counter("agent.llm.task_budget.rejected")
	llmFallbackSucceeded, _ := meter.Int64Counter("agent.llm.fallback.succeeded")
	llmFallbackFailed, _ := meter.Int64Counter("agent.llm.fallback.failed")
	answerPipelineRuns, _ := meter.Int64Counter("answer_pipeline.runs")
	answerPipelineStages, _ := meter.Int64Counter("answer_pipeline.stage.runs")
	answerPipelineDuration, _ := meter.Float64Histogram("answer_pipeline.stage.duration_ms")
	answerPipelineTokens, _ := meter.Int64Counter("answer_pipeline.stage.tokens")
	answerPipelineWarnings, _ := meter.Int64Counter("answer_pipeline.warnings")
	answerPipelineConfidence, _ := meter.Int64Counter("answer_pipeline.confidence")
	multiAgentRoutes, _ := meter.Int64Counter("agent.multiagent.route.selections")
	multiAgentPhases, _ := meter.Int64Counter("agent.multiagent.phase.calls")
	multiAgentPhaseLatency, _ := meter.Float64Histogram("agent.multiagent.phase.latency_ms")
	multiAgentCriticReviews, _ := meter.Int64Counter("agent.multiagent.critic.reviews")
	multiAgentCriticReplans, _ := meter.Int64Counter("agent.multiagent.critic.replans")
	multiAgentCheckpoints, _ := meter.Int64Counter("agent.multiagent.verifier.checkpoints")
	multiAgentResumes, _ := meter.Int64Counter("agent.multiagent.verifier.resumes")
	multiAgentConfigChanges, _ := meter.Int64Counter("agent.multiagent.team_config.changes")
	multiAgentTeamSelections, _ := meter.Int64Counter("agent.multiagent.team.selections")
	multiAgentTeamConfigEvents, _ := meter.Int64Counter("agent.multiagent.team_config.events")
	multiAgentRuntimeCalls, _ := meter.Int64Counter("agent.multiagent.runtime.calls")
	multiAgentRuntimeLatency, _ := meter.Float64Histogram("agent.multiagent.runtime.latency_ms")
	multiAgentRuntimeFallbacks, _ := meter.Int64Counter("agent.multiagent.runtime.fallbacks")
	multiAgentRuntimeEvents, _ := meter.Int64Counter("agent.multiagent.runtime.events")
	retrievalCalls, _ := meter.Int64Counter("agent.store.retrieval.calls")
	retrievalFailures, _ := meter.Int64Counter("agent.store.retrieval.failures")
	retrievalFallbacks, _ := meter.Int64Counter("agent.store.retrieval.fallbacks")
	retrievalSlowPhases, _ := meter.Int64Counter("agent.store.retrieval.slow_phases")
	retrievalItems, _ := meter.Int64Counter("agent.store.retrieval.items")
	retrievalLatency, _ := meter.Float64Histogram("agent.store.retrieval.latency_ms")
	approvalEvents, _ := meter.Int64Counter("agent.approval.events")

	return &Collector{
		plannerCalls:               plannerCalls,
		plannerFailures:            plannerFailures,
		plannerLatencyMs:           plannerLatencyMs,
		writerCalls:                writerCalls,
		writerFailures:             writerFailures,
		writerLatencyMs:            writerLatencyMs,
		executorCalls:              executorCalls,
		executorFailures:           executorFailures,
		executorLatencyMs:          executorLatencyMs,
		runAllCalls:                runAllCalls,
		tasksCompleted:             tasksCompleted,
		fallbackHits:               fallbackHits,
		promptTokens:               promptTokens,
		completionTokens:           completionTokens,
		totalTokens:                totalTokens,
		llmSceneCalls:              llmSceneCalls,
		llmSceneErrors:             llmSceneErrors,
		llmSceneLatency:            llmSceneLatency,
		llmScenePromptTokens:       llmScenePromptTokens,
		llmSceneCompletionTokens:   llmSceneCompletionTokens,
		llmSceneTotalTokens:        llmSceneTotalTokens,
		llmSceneEstimatedCost:      llmSceneEstimatedCost,
		llmCircuitOpened:           llmCircuitOpened,
		llmCircuitRejected:         llmCircuitRejected,
		llmRetryBudgetExhausted:    llmRetryBudgetExhausted,
		llmTaskBudgetRejected:      llmTaskBudgetRejected,
		llmFallbackSucceeded:       llmFallbackSucceeded,
		llmFallbackFailed:          llmFallbackFailed,
		answerPipelineRuns:         answerPipelineRuns,
		answerPipelineStages:       answerPipelineStages,
		answerPipelineDuration:     answerPipelineDuration,
		answerPipelineTokens:       answerPipelineTokens,
		answerPipelineWarnings:     answerPipelineWarnings,
		answerPipelineConfidence:   answerPipelineConfidence,
		multiAgentRoutes:           multiAgentRoutes,
		multiAgentPhases:           multiAgentPhases,
		multiAgentPhaseLatency:     multiAgentPhaseLatency,
		multiAgentCriticReviews:    multiAgentCriticReviews,
		multiAgentCriticReplans:    multiAgentCriticReplans,
		multiAgentCheckpoints:      multiAgentCheckpoints,
		multiAgentResumes:          multiAgentResumes,
		multiAgentConfigChanges:    multiAgentConfigChanges,
		multiAgentTeamSelections:   multiAgentTeamSelections,
		multiAgentTeamConfigEvents: multiAgentTeamConfigEvents,
		multiAgentRuntimeCalls:     multiAgentRuntimeCalls,
		multiAgentRuntimeLatency:   multiAgentRuntimeLatency,
		multiAgentRuntimeFallbacks: multiAgentRuntimeFallbacks,
		multiAgentRuntimeEvents:    multiAgentRuntimeEvents,
		retrievalCalls:             retrievalCalls,
		retrievalFailures:          retrievalFailures,
		retrievalFallbacks:         retrievalFallbacks,
		retrievalSlowPhases:        retrievalSlowPhases,
		retrievalItems:             retrievalItems,
		retrievalLatency:           retrievalLatency,
		approvalEvents:             approvalEvents,
	}
}

// ObserveDurableApproval records one bounded-cardinality lifecycle event.
func (c *Collector) ObserveDurableApproval(ctx context.Context, event string) {
	switch event {
	case "created", "approved", "rejected", "consumed", "expired", "conflict", "recovery_success", "recovery_failure":
	default:
		event = "unknown"
	}
	c.mu.Lock()
	switch event {
	case "created":
		c.s.DurableApprovalsCreated++
	case "approved":
		c.s.DurableApprovalsApproved++
	case "rejected":
		c.s.DurableApprovalsRejected++
	case "consumed":
		c.s.DurableApprovalsConsumed++
	case "expired":
		c.s.DurableApprovalsExpired++
	case "conflict":
		c.s.DurableApprovalConflicts++
	case "recovery_success":
		c.s.DurableApprovalRecoverySuccesses++
	case "recovery_failure":
		c.s.DurableApprovalRecoveryFailures++
	}
	c.mu.Unlock()
	c.approvalEvents.Add(ctx, 1, api.WithAttributes(attribute.String("event", event)))
}

func (c *Collector) ObserveApprovalCleanup(ctx context.Context, deleted int64, err error) {
	if deleted < 0 {
		deleted = 0
	}
	c.mu.Lock()
	c.s.DurableApprovalsCleaned += deleted
	if err != nil {
		c.s.DurableApprovalCleanupFailures++
	}
	c.mu.Unlock()
	if deleted > 0 {
		c.approvalEvents.Add(ctx, deleted, api.WithAttributes(attribute.String("event", "cleaned")))
	}
	if err != nil {
		c.approvalEvents.Add(ctx, 1, api.WithAttributes(attribute.String("event", "cleanup_failure")))
	}
}

// ObserveRetrieval records one bounded-cardinality ParadeDB retrieval phase.
func (c *Collector) ObserveRetrieval(ctx context.Context, stage string, latency time.Duration, items int, slow bool, err error) {
	switch stage {
	case "bm25", "pgvector", "rrf":
	default:
		stage = "unknown"
	}
	c.mu.Lock()
	c.s.RetrievalCalls++
	c.s.RetrievalItems += int64(items)
	c.s.RetrievalLatencySum += latency
	switch stage {
	case "bm25":
		c.s.RetrievalBM25Calls++
		c.s.RetrievalBM25Items += int64(items)
		c.s.RetrievalBM25LatencySum += latency
		if err != nil {
			c.s.RetrievalBM25Failures++
		}
	case "pgvector":
		c.s.RetrievalPGVectorCalls++
		c.s.RetrievalPGVectorItems += int64(items)
		c.s.RetrievalPGVectorLatencySum += latency
		if err != nil {
			c.s.RetrievalPGVectorFailures++
		}
	case "rrf":
		c.s.RetrievalRRFCalls++
		c.s.RetrievalRRFItems += int64(items)
		c.s.RetrievalRRFLatencySum += latency
		if err != nil {
			c.s.RetrievalRRFFailures++
		}
	}
	if err != nil {
		c.s.RetrievalFailures++
	}
	if slow {
		c.s.RetrievalSlowPhases++
	}
	c.mu.Unlock()
	attrs := api.WithAttributes(attribute.String("stage", stage))
	c.retrievalCalls.Add(ctx, 1, attrs)
	c.retrievalItems.Add(ctx, int64(items), attrs)
	c.retrievalLatency.Record(ctx, float64(latency.Microseconds())/1000, attrs)
	if err != nil {
		c.retrievalFailures.Add(ctx, 1, attrs)
	}
	if slow {
		c.retrievalSlowPhases.Add(ctx, 1, attrs)
	}
}

func (c *Collector) IncRetrievalFallback(ctx context.Context) {
	c.mu.Lock()
	c.s.RetrievalFallbacks++
	c.mu.Unlock()
	c.retrievalFallbacks.Add(ctx, 1)
}

func (c *Collector) ObserveAnswerPipeline(mode string, report *types.AnswerAuditReport) {
	if report == nil {
		return
	}
	status := "completed"
	if !report.Publishable {
		status = "not_publishable"
	}
	c.mu.Lock()
	c.s.AnswerPipelineRuns++
	c.s.AnswerPipelineStages += int64(len(report.Stages))
	c.mu.Unlock()
	ctx := context.Background()
	c.answerPipelineRuns.Add(ctx, 1, api.WithAttributes(attribute.String("mode", mode), attribute.String("status", status), attribute.String("enforcement", report.Enforcement)))
	for _, stage := range report.Stages {
		attrs := api.WithAttributes(attribute.String("stage", stage.Name), attribute.String("status", stage.Status), attribute.String("reason", stage.Reason))
		c.answerPipelineStages.Add(ctx, 1, attrs)
		c.answerPipelineDuration.Record(ctx, float64(stage.DurationMS), attrs)
		c.answerPipelineTokens.Add(ctx, int64(stage.TokenUsage.TotalTokens), attrs)
		if stage.Status == "warned" {
			c.mu.Lock()
			c.s.AnswerPipelineWarnings++
			c.mu.Unlock()
		}
		for _, finding := range stage.Findings {
			c.answerPipelineWarnings.Add(ctx, 1, api.WithAttributes(attribute.String("kind", finding.Kind), attribute.String("stage", stage.Name)))
		}
	}
	if report.FinalConfidence != "" {
		c.answerPipelineConfidence.Add(ctx, 1, api.WithAttributes(attribute.String("level", report.FinalConfidence), attribute.String("mode", mode)))
	}
}

func (c *Collector) ObserveLLMCall(event llmcore.CallEvent) {
	c.ObserveLLMCallContext(context.Background(), event)
}

func (c *Collector) ObserveLLMCallContext(ctx context.Context, event llmcore.CallEvent) {
	attrs := api.WithAttributes(attribute.String("llm.scene", event.Scene), attribute.String("llm.provider", event.Provider), attribute.String("llm.model", event.Model), attribute.Int("llm.attempts", event.Attempts), attribute.Bool("llm.fallback_used", event.FallbackUsed))
	c.mu.Lock()
	c.s.LLMSceneCalls++
	c.s.LLMPromptTokens += int64(event.Usage.PromptTokens)
	c.s.LLMCompletionTokens += int64(event.Usage.CompletionTokens)
	c.s.LLMTotalTokens += int64(event.Usage.TotalTokens)
	c.s.LLMEstimatedCostUSD += event.EstimatedCostUSD
	if event.Err != nil {
		c.s.LLMSceneErrors++
	}
	c.mu.Unlock()
	c.llmSceneCalls.Add(ctx, 1, attrs)
	c.llmSceneLatency.Record(ctx, float64(event.Duration.Milliseconds()), attrs)
	c.llmScenePromptTokens.Add(ctx, int64(event.Usage.PromptTokens), attrs)
	c.llmSceneCompletionTokens.Add(ctx, int64(event.Usage.CompletionTokens), attrs)
	c.llmSceneTotalTokens.Add(ctx, int64(event.Usage.TotalTokens), attrs)
	c.llmSceneEstimatedCost.Add(ctx, event.EstimatedCostUSD, attrs)
	if event.Err != nil {
		c.llmSceneErrors.Add(ctx, 1, attrs)
	}
}

func (c *Collector) ObserveLLMReliability(ctx context.Context, event llmcore.ReliabilityEvent) {
	attrs := api.WithAttributes(attribute.String("llm.scene", event.Scene), attribute.String("llm.provider", event.Provider), attribute.String("llm.model", event.Model), attribute.String("llm.fallback_scene", event.FallbackScene))
	c.mu.Lock()
	switch event.Kind {
	case llmcore.ReliabilityCircuitOpened:
		c.s.LLMCircuitOpened++
	case llmcore.ReliabilityCircuitRejected:
		c.s.LLMCircuitRejected++
	case llmcore.ReliabilityRetryBudgetExhausted:
		c.s.LLMRetryBudgetExhausted++
	case llmcore.ReliabilityTaskBudgetRejected:
		c.s.LLMTaskBudgetRejected++
	case llmcore.ReliabilityFallbackSucceeded:
		c.s.LLMFallbackSucceeded++
	case llmcore.ReliabilityFallbackFailed:
		c.s.LLMFallbackFailed++
	}
	c.mu.Unlock()
	switch event.Kind {
	case llmcore.ReliabilityCircuitOpened:
		c.llmCircuitOpened.Add(ctx, 1, attrs)
	case llmcore.ReliabilityCircuitRejected:
		c.llmCircuitRejected.Add(ctx, 1, attrs)
	case llmcore.ReliabilityRetryBudgetExhausted:
		c.llmRetryBudgetExhausted.Add(ctx, 1, attrs)
	case llmcore.ReliabilityTaskBudgetRejected:
		c.llmTaskBudgetRejected.Add(ctx, 1, attrs)
	case llmcore.ReliabilityFallbackSucceeded:
		c.llmFallbackSucceeded.Add(ctx, 1, attrs)
	case llmcore.ReliabilityFallbackFailed:
		c.llmFallbackFailed.Add(ctx, 1, attrs)
	}
}

func (c *Collector) ObservePlanner(latency time.Duration, err error) {
	c.mu.Lock()
	c.s.PlannerCalls++
	c.s.PlannerLatencySum += latency
	if err != nil {
		c.s.PlannerFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	c.plannerCalls.Add(ctx, 1)
	c.plannerLatencyMs.Record(ctx, float64(latency.Milliseconds()))
	if err != nil {
		c.plannerFailures.Add(ctx, 1)
	}
}

// ObserveWriter records a WriterAgent LLM call duration and outcome.
// It uses dedicated writer metrics so writer latency is not conflated with
// planner latency in dashboards and alerts.
func (c *Collector) ObserveWriter(latency time.Duration, err error) {
	c.mu.Lock()
	c.s.WriterCalls++
	c.s.WriterLatencySum += latency
	if err != nil {
		c.s.WriterFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	c.writerCalls.Add(ctx, 1)
	c.writerLatencyMs.Record(ctx, float64(latency.Milliseconds()))
	if err != nil {
		c.writerFailures.Add(ctx, 1)
	}
}

func (c *Collector) ObserveExecutor(latency time.Duration, err error, action string) {
	c.mu.Lock()
	c.s.ExecutorCalls++
	c.s.ExecutorLatencySum += latency
	if err != nil {
		c.s.ExecutorFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	attrs := api.WithAttributes(attribute.String("action", action))
	c.executorCalls.Add(ctx, 1, attrs)
	c.executorLatencyMs.Record(ctx, float64(latency.Milliseconds()), attrs)
	if err != nil {
		c.executorFailures.Add(ctx, 1, attrs)
	}
}

func (c *Collector) IncRunAll() {
	c.mu.Lock()
	c.s.RunAllCalls++
	c.mu.Unlock()

	c.runAllCalls.Add(context.Background(), 1)
}

func (c *Collector) IncCompleted() {
	c.mu.Lock()
	c.s.TasksCompleted++
	c.mu.Unlock()

	c.tasksCompleted.Add(context.Background(), 1)
}

func (c *Collector) IncFallbackHit() {
	c.mu.Lock()
	c.s.FallbackHits++
	c.mu.Unlock()

	c.fallbackHits.Add(context.Background(), 1)
}

func (c *Collector) ObserveTokens(prompt, completion, total int, role string) {
	c.mu.Lock()
	c.s.PromptTokens += int64(prompt)
	c.s.CompletionTokens += int64(completion)
	c.s.TotalTokens += int64(total)
	c.mu.Unlock()

	ctx := context.Background()
	attrs := api.WithAttributes(attribute.String("role", role))
	c.promptTokens.Add(ctx, int64(prompt), attrs)
	c.completionTokens.Add(ctx, int64(completion), attrs)
	c.totalTokens.Add(ctx, int64(total), attrs)
}

func (c *Collector) ObserveMultiAgentRoute(configured, effective, reason string) {
	category := multiAgentRouteCategory(reason)
	c.mu.Lock()
	c.s.MultiAgentRoutes++
	if category == "budget_fallback" {
		c.s.MultiAgentBudgetFallbacks++
	}
	if category == "escalation" {
		c.s.MultiAgentEscalations++
	}
	c.mu.Unlock()
	c.multiAgentRoutes.Add(context.Background(), 1, api.WithAttributes(
		attribute.String("configured_workflow", configured),
		attribute.String("effective_workflow", effective),
		attribute.String("reason_category", category),
	))
}

// ObserveMultiAgentRuntime records the rollout denominator, outcome, and
// latency separately for DAG and Legacy without task-level attributes.
func (c *Collector) ObserveMultiAgentRuntime(runtime, outcome string, latency time.Duration) {
	if runtime != "dag" {
		runtime = "legacy"
	}
	if outcome != "success" && outcome != "partial" && outcome != "failure" && outcome != "canceled" {
		outcome = "failure"
	}
	c.mu.Lock()
	if runtime == "dag" {
		c.s.MultiAgentDAGCalls++
		c.s.MultiAgentDAGLatencySum += latency
		if outcome == "success" {
			c.s.MultiAgentDAGCompletions++
		} else if outcome != "partial" {
			c.s.MultiAgentDAGFailures++
		}
	} else {
		c.s.MultiAgentLegacyCalls++
		c.s.MultiAgentLegacyLatencySum += latency
		if outcome == "success" {
			c.s.MultiAgentLegacyCompletions++
		} else if outcome != "partial" {
			c.s.MultiAgentLegacyFailures++
		}
	}
	c.mu.Unlock()
	attrs := api.WithAttributes(attribute.String("runtime", runtime), attribute.String("outcome", outcome))
	c.multiAgentRuntimeCalls.Add(context.Background(), 1, attrs)
	c.multiAgentRuntimeLatency.Record(context.Background(), float64(latency.Milliseconds()), attrs)
}

func (c *Collector) ObserveMultiAgentRuntimeFallback(reason string) {
	category := multiAgentRouteCategory(reason)
	c.mu.Lock()
	c.s.MultiAgentDAGFallbacks++
	c.mu.Unlock()
	c.multiAgentRuntimeFallbacks.Add(context.Background(), 1, api.WithAttributes(attribute.String("reason_category", category)))
}

// ObserveMultiAgentRuntimeEvent records bounded task-level rollout signals.
func (c *Collector) ObserveMultiAgentRuntimeEvent(runtime, event string) {
	if runtime != "dag" {
		runtime = "legacy"
	}
	if event != "approval_required" && event != "replanned" && event != "observed" {
		return
	}
	c.mu.Lock()
	if runtime == "dag" {
		if event == "observed" {
			c.s.MultiAgentDAGEventsObserved++
		} else if event == "approval_required" {
			c.s.MultiAgentDAGApprovalRequired++
		} else {
			c.s.MultiAgentDAGReplanned++
		}
	} else {
		if event == "observed" {
			c.s.MultiAgentLegacyEventsObserved++
		} else if event == "approval_required" {
			c.s.MultiAgentLegacyApprovalRequired++
		} else {
			c.s.MultiAgentLegacyReplanned++
		}
	}
	c.mu.Unlock()
	c.multiAgentRuntimeEvents.Add(context.Background(), 1, api.WithAttributes(
		attribute.String("runtime", runtime),
		attribute.String("event", event),
	))
}

func multiAgentRouteCategory(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.HasPrefix(reason, "dag_fallback:"):
		return "dag_fallback"
	case strings.HasPrefix(reason, "resume_escalation:"), strings.HasPrefix(reason, "adaptive_replan:"), strings.HasPrefix(reason, "execution_replan:"):
		return "escalation"
	case strings.Contains(reason, "budget_fallback:"):
		return "budget_fallback"
	case strings.Contains(reason, "high_risk_action:"):
		return "high_risk"
	case strings.Contains(reason, "complexity:"):
		return "complexity"
	case strings.Contains(reason, "intent:"):
		return "intent"
	case strings.Contains(reason, "plan_steps"):
		return "plan_steps"
	case strings.HasPrefix(reason, "persisted:"):
		return "persisted"
	case reason == "configured":
		return "configured"
	default:
		return "default"
	}
}

func (c *Collector) ObserveMultiAgentPhase(phase, outcome string, latency time.Duration) {
	failure := outcome == "error" || outcome == "rejected"
	c.mu.Lock()
	c.s.MultiAgentPhaseCalls++
	c.s.MultiAgentPhaseLatencySum += latency
	if failure {
		c.s.MultiAgentPhaseFailures++
	}
	c.mu.Unlock()
	attrs := api.WithAttributes(attribute.String("phase", phase), attribute.String("outcome", outcome))
	c.multiAgentPhases.Add(context.Background(), 1, attrs)
	c.multiAgentPhaseLatency.Record(context.Background(), float64(latency.Milliseconds()), attrs)
}

func (c *Collector) ObserveMultiAgentCriticReview(outcome string) {
	c.mu.Lock()
	switch outcome {
	case "approved":
		c.s.MultiAgentCriticApprovals++
	case "rejected":
		c.s.MultiAgentCriticRejections++
	default:
		c.s.MultiAgentCriticErrors++
		outcome = "error"
	}
	c.mu.Unlock()
	c.multiAgentCriticReviews.Add(context.Background(), 1, api.WithAttributes(attribute.String("outcome", outcome)))
}

func (c *Collector) IncMultiAgentCriticReplan() {
	c.mu.Lock()
	c.s.MultiAgentCriticReplans++
	c.mu.Unlock()
	c.multiAgentCriticReplans.Add(context.Background(), 1)
}

func (c *Collector) ObserveMultiAgentVerifierCheckpoint(outcome string) {
	c.mu.Lock()
	c.s.MultiAgentCheckpoints++
	c.mu.Unlock()
	c.multiAgentCheckpoints.Add(context.Background(), 1, api.WithAttributes(attribute.String("outcome", outcome)))
}

func (c *Collector) ObserveMultiAgentVerifierResume(outcome string) {
	c.mu.Lock()
	c.s.MultiAgentResumeAttempts++
	if outcome == "success" {
		c.s.MultiAgentResumeSuccesses++
	} else {
		c.s.MultiAgentResumeFailures++
	}
	c.mu.Unlock()
	c.multiAgentResumes.Add(context.Background(), 1, api.WithAttributes(attribute.String("outcome", outcome)))
}

func (c *Collector) ObserveMultiAgentConfigChange(policy, outcome string) {
	c.mu.Lock()
	c.s.MultiAgentConfigChanges++
	if outcome == "blocked" {
		c.s.MultiAgentConfigBlocks++
	} else if outcome == "migrated" {
		c.s.MultiAgentConfigMigrations++
	}
	c.mu.Unlock()
	c.multiAgentConfigChanges.Add(context.Background(), 1, api.WithAttributes(
		attribute.String("policy", policy),
		attribute.String("outcome", outcome),
	))
}

// ObserveMultiAgentTeamSelection records task-creation routing outcomes. Team
// names come from the bounded teams.yaml configuration and are safe metric labels.
func (c *Collector) ObserveMultiAgentTeamSelection(ctx context.Context, team, outcome, source string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if outcome != "created" && outcome != "forbidden" && outcome != "draining" && outcome != "retired" {
		return
	}
	switch source {
	case "explicit", "tenant_default", "global_default":
	default:
		source = "unknown"
	}
	usedDefault := source == "tenant_default" || source == "global_default"
	c.mu.Lock()
	if outcome == "created" {
		c.s.MultiAgentTeamTasksCreated++
		if c.s.MultiAgentTeamTasksCreatedByTeam == nil {
			c.s.MultiAgentTeamTasksCreatedByTeam = make(map[string]int64)
		}
		c.s.MultiAgentTeamTasksCreatedByTeam[team]++
		if c.s.MultiAgentTeamTasksCreatedBySource == nil {
			c.s.MultiAgentTeamTasksCreatedBySource = make(map[string]int64)
		}
		c.s.MultiAgentTeamTasksCreatedBySource[source]++
	} else {
		c.s.MultiAgentTeamSelectionRejections++
		if outcome == "draining" {
			c.s.MultiAgentTeamDrainingRejections++
		} else if outcome == "retired" {
			c.s.MultiAgentTeamRetiredRejections++
		}
		if c.s.MultiAgentTeamRejectionsBySource == nil {
			c.s.MultiAgentTeamRejectionsBySource = make(map[string]int64)
		}
		c.s.MultiAgentTeamRejectionsBySource[source]++
		if usedDefault {
			c.s.MultiAgentTeamDefaultUnavailable++
		}
	}
	c.mu.Unlock()
	c.multiAgentTeamSelections.Add(ctx, 1, api.WithAttributes(
		attribute.String("team", team),
		attribute.String("outcome", outcome),
		attribute.String("source", source),
		attribute.Bool("used_default", usedDefault),
	))
}

// ObserveMultiAgentTeamConfigEvent records bounded Team configuration events.
func (c *Collector) ObserveMultiAgentTeamConfigEvent(ctx context.Context, event string) {
	switch event {
	case "readiness_failure", "reload_rejected", "lifecycle_changed", "lifecycle_conflict", "default_protected", "audit_archived", "audit_archive_conflict", "audit_capacity_rejected", "audit_integrity_failure":
	default:
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	switch event {
	case "readiness_failure":
		c.s.MultiAgentTeamReadinessFailures++
	case "reload_rejected":
		c.s.MultiAgentTeamReloadRejections++
	case "lifecycle_changed":
		c.s.MultiAgentTeamLifecycleChanges++
	case "lifecycle_conflict":
		c.s.MultiAgentTeamLifecycleConflicts++
	case "default_protected":
		c.s.MultiAgentTeamDefaultProtections++
	case "audit_archived":
		c.s.MultiAgentTeamAuditArchives++
	case "audit_archive_conflict":
		c.s.MultiAgentTeamAuditArchiveConflicts++
	case "audit_capacity_rejected":
		c.s.MultiAgentTeamAuditCapacityRejections++
	case "audit_integrity_failure":
		c.s.MultiAgentTeamAuditIntegrityFailures++
	}
	c.mu.Unlock()
	c.multiAgentTeamConfigEvents.Add(ctx, 1, api.WithAttributes(attribute.String("event", event)))
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.s
	if c.s.MultiAgentTeamTasksCreatedByTeam != nil {
		snapshot.MultiAgentTeamTasksCreatedByTeam = make(map[string]int64, len(c.s.MultiAgentTeamTasksCreatedByTeam))
		for team, count := range c.s.MultiAgentTeamTasksCreatedByTeam {
			snapshot.MultiAgentTeamTasksCreatedByTeam[team] = count
		}
	}
	if c.s.MultiAgentTeamTasksCreatedBySource != nil {
		snapshot.MultiAgentTeamTasksCreatedBySource = make(map[string]int64, len(c.s.MultiAgentTeamTasksCreatedBySource))
		for source, count := range c.s.MultiAgentTeamTasksCreatedBySource {
			snapshot.MultiAgentTeamTasksCreatedBySource[source] = count
		}
	}
	if c.s.MultiAgentTeamRejectionsBySource != nil {
		snapshot.MultiAgentTeamRejectionsBySource = make(map[string]int64, len(c.s.MultiAgentTeamRejectionsBySource))
		for source, count := range c.s.MultiAgentTeamRejectionsBySource {
			snapshot.MultiAgentTeamRejectionsBySource[source] = count
		}
	}
	snapshot.RetrievalAverageLatencyMS = averageDurationMS(snapshot.RetrievalLatencySum, snapshot.RetrievalCalls)
	snapshot.RetrievalBM25AverageLatencyMS = averageDurationMS(snapshot.RetrievalBM25LatencySum, snapshot.RetrievalBM25Calls)
	snapshot.RetrievalPGVectorAverageLatencyMS = averageDurationMS(snapshot.RetrievalPGVectorLatencySum, snapshot.RetrievalPGVectorCalls)
	snapshot.RetrievalRRFAverageLatencyMS = averageDurationMS(snapshot.RetrievalRRFLatencySum, snapshot.RetrievalRRFCalls)
	return snapshot
}

func averageDurationMS(total time.Duration, count int64) float64 {
	if count <= 0 {
		return 0
	}
	return float64(total) / float64(time.Millisecond) / float64(count)
}
