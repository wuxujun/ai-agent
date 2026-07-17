package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/answerpipeline"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/diagnostics"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/testgen"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/model"
)

type Engine struct {
	AnswerPipeline              answerpipeline.Pipeline
	Planner                     planner.Planner
	Finalizer                   planner.TaskFinalizer
	CitationVerifier            planner.CitationVerifier
	SafetyGuard                 policy.SafetyGuard
	IntentRouter                planner.IntentRouter
	MemoryConflictResolver      memory.ConflictResolver
	CodeReviewer                review.CodeReviewer
	CollectCodeChanges          func(context.Context, string) (review.ChangeSet, error)
	TestGenerator               testgen.Generator
	FailureDiagnoser            diagnostics.Diagnoser
	PlanCritic                  plancritic.Critic
	PromptInjectionDetector     promptguard.Detector
	EvidenceRelevanceFilter     evidencefilter.Filter
	EvidenceConflictResolver    evidenceconflict.Resolver
	SourceCredibilityScorer     sourcecredibility.Scorer
	FactFreshnessChecker        factfreshness.Checker
	NumericConsistencyChecker   numericconsistency.Checker
	AnswerUncertaintyCalibrator uncertainty.Calibrator
	Executor                    executor.Executor
	Metrics                     *metrics.Collector
	Mode                        Mode
	AdkModel                    model.LLM
	LLMSceneEnabled             func(string) bool
	// Coordinator is required when Mode == ModeMultiAgent.
	Coordinator *multiagent.Coordinator
	// Store handles database persistence and long-term memory.
	Store store.Store

	// Approvals is the in-memory approval state store for this engine instance.
	// When nil, the engine falls back to defaultApprovals (the process-wide
	// singleton). Tests that require isolation should set this to a fresh
	// NewApprovalStore() before use.
	//
	// P1-1: replacing the implicit global-state coupling with an explicit field
	// so SuspendForApproval no longer depends on package-level variables.
	Approvals *ApprovalStore

	// ApprovalBus enables cross-instance approval and cancel signalling via
	// Redis Pub/Sub. When nil the engine operates with in-process channels only
	// (single-instance mode). When set, remote approve/reject signals published
	// by any peer instance are forwarded into the local approval channel by the
	// bus's background loop, so SuspendForApproval's select sees them without
	// any extra logic.
	ApprovalBus *ApprovalBus

	// einoRunner is compiled once and cached for the lifetime of the Engine.
	// A sync.RWMutex guards lazy initialisation; unlike sync.Once, a failed
	// compilation attempt can be retried on the next call.
	//
	// Hot path (runner already compiled): acquired as RLock so concurrent
	// requests do not block each other at all.
	// Cold path (first compile or retry): upgraded to full Lock with a
	// double-check to prevent redundant compilations.
	einoMu     sync.RWMutex
	einoRunner any  // compose.Runnable[*einoStepState, *types.Task] after successful compile
	einoReady  bool // true once einoRunner has been successfully compiled

	EventCallback    func(taskID string, status types.TaskStatus)
	ApprovalCallback func(taskID string, approval *types.ApprovalRequest)
	StepCallback     func(taskID string, status types.TaskStatus, step *types.StepTrace)
	TokenCallback    func(taskID string, token string)

	// adkRunner is compiled once and cached for reuse across all runAdkNext calls.
	adkOnce   sync.Once
	adkRunner any // *runner.Runner
	adkErr    error
}

func (e *Engine) llmSceneEnabled(scene string) bool {
	if e.LLMSceneEnabled != nil {
		return e.LLMSceneEnabled(scene)
	}
	_, enabled := config.Get().LLM.Scenes[scene]
	return enabled
}

func (e *Engine) finalizeAnswer(ctx context.Context, task *types.Task, fallback string) (string, types.TokenUsage) {
	answer, usage, _ := e.finalizeAnswerDetailed(ctx, task, fallback)
	return answer, usage
}

func (e *Engine) finalizeAnswerDetailed(ctx context.Context, task *types.Task, fallback string) (string, types.TokenUsage, string) {
	if e.Finalizer == nil {
		engineLog.Warn("task finalizer unavailable", "task_id", task.ID, "reason", "not_initialized")
		return fallback, types.TokenUsage{}, "not_initialized"
	}
	if !e.llmSceneEnabled(config.LLMSceneTaskFinalizer) {
		engineLog.Warn("task finalizer unavailable", "task_id", task.ID, "reason", "scene_disabled", "scene", config.LLMSceneTaskFinalizer)
		return fallback, types.TokenUsage{}, "scene_disabled"
	}
	if !llmcore.AllowedForTask(config.LLMSceneTaskFinalizer, task) {
		engineLog.Warn("task finalizer unavailable", "task_id", task.ID, "reason", "token_reserve", "scene", config.LLMSceneTaskFinalizer, "token_budget", task.TokenBudget)
		return fallback, types.TokenUsage{}, "token_reserve"
	}
	answer, usage, err := e.Finalizer.Finalize(ctx, task)
	if err != nil {
		engineLog.Warn("task finalizer failed; using fallback result", "task_id", task.ID, "reason", "provider_error", "error", err)
		return fallback, types.TokenUsage{}, "provider_error"
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "finalizer")
	}
	return answer, usage, ""
}

func (e *Engine) verifyCitations(ctx context.Context, task *types.Task) {
	if e.CitationVerifier == nil || !e.llmSceneEnabled(config.LLMSceneCitationVerifier) {
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneCitationVerifier, task) || !planner.HasCitationEvidence(task) {
		return
	}
	result, usage, err := e.CitationVerifier.Verify(ctx, task, task.FinalAnswer)
	if err != nil {
		engineLog.Warn("citation verifier failed; keeping original answer", "task_id", task.ID, "error", err)
		return
	}
	if result == nil || strings.TrimSpace(result.VerifiedAnswer) == "" {
		engineLog.Warn("citation verifier returned no answer; keeping original answer", "task_id", task.ID)
		return
	}
	task.FinalAnswer = result.VerifiedAnswer
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Action:      "citation_verify",
		Observation: fmt.Sprintf("supported=%t unsupported_claims=%d citation_issues=%d", result.Supported, len(result.UnsupportedClaims), len(result.CitationIssues)),
		TokenUsage:  usage,
	})
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "citation_verifier")
	}
}

func (e *Engine) reviewCodeChanges(ctx context.Context, task *types.Task) {
	if !e.codeReviewEligible(task) || !review.TaskMayHaveCodeChanges(task) {
		return
	}
	changes, err := e.collectChanges(ctx, task.Workspace)
	if err != nil {
		engineLog.Warn("code review change collection failed; skipping review", "task_id", task.ID, "error", err)
		return
	}
	if strings.TrimSpace(changes.Diff) == "" || len(changes.Paths) == 0 {
		return
	}
	e.reviewCodeChangesWithSet(ctx, task, changes)
}

func (e *Engine) codeReviewEligible(task *types.Task) bool {
	return e.CodeReviewer != nil && e.llmSceneEnabled(config.LLMSceneCodeReviewer) && !taskHasAction(task, "code_review") && llmcore.AllowedForTask(config.LLMSceneCodeReviewer, task)
}

func (e *Engine) reviewCodeChangesWithSet(ctx context.Context, task *types.Task, changes review.ChangeSet) {
	result, usage, err := e.CodeReviewer.Review(ctx, task, changes)
	trace := types.StepTrace{Step: task.StepCount, Action: "code_review", TokenUsage: usage}
	if err != nil {
		trace.Observation = "review_failed; final answer preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("code reviewer failed; keeping original answer", "task_id", task.ID, "error", err)
		e.observeCodeReviewUsage(usage)
		return
	}
	if result == nil {
		trace.Observation = "review_failed; final answer preserved"
		task.Trace = append(task.Trace, trace)
		e.observeCodeReviewUsage(usage)
		return
	}
	trace.Observation = fmt.Sprintf("findings=%d summary=%s", len(result.Findings), result.Summary)
	for _, finding := range result.Findings {
		trace.Evidence = append(trace.Evidence, types.Evidence{Path: finding.Path, Query: "code review", Lines: []string{fmt.Sprintf("[%s] line %d: %s: %s", finding.Severity, finding.Line, finding.Title, finding.Detail)}})
	}
	task.Trace = append(task.Trace, trace)
	e.observeCodeReviewUsage(usage)
	if len(result.Findings) == 0 {
		return
	}
	var section strings.Builder
	section.WriteString("\n\n## Code review\n")
	for _, finding := range result.Findings {
		location := finding.Path
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, finding.Line)
		}
		fmt.Fprintf(&section, "- [%s] `%s` %s: %s\n", strings.ToUpper(finding.Severity), location, finding.Title, finding.Detail)
	}
	task.FinalAnswer = strings.TrimSpace(task.FinalAnswer) + strings.TrimRight(section.String(), "\n")
}

func (e *Engine) observeCodeReviewUsage(usage types.TokenUsage) {
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "code_reviewer")
	}
}

func (e *Engine) generateTestSuggestions(ctx context.Context, task *types.Task) {
	if !e.testGenerationEligible(task) || !review.TaskMayHaveCodeChanges(task) {
		return
	}
	changes, err := e.collectChanges(ctx, task.Workspace)
	if err != nil {
		engineLog.Warn("test generation change collection failed; skipping", "task_id", task.ID, "error", err)
		return
	}
	if strings.TrimSpace(changes.Diff) == "" || len(changes.Paths) == 0 {
		return
	}
	e.generateTestSuggestionsWithSet(ctx, task, changes)
}

func (e *Engine) testGenerationEligible(task *types.Task) bool {
	return e.TestGenerator != nil && e.llmSceneEnabled(config.LLMSceneTestGenerator) && !taskHasAction(task, "test_generate") && llmcore.AllowedForTask(config.LLMSceneTestGenerator, task)
}

func (e *Engine) generateTestSuggestionsWithSet(ctx context.Context, task *types.Task, changes review.ChangeSet) {
	result, usage, err := e.TestGenerator.Generate(ctx, task, changes)
	trace := types.StepTrace{Step: task.StepCount, Action: "test_generate", TokenUsage: usage}
	if err != nil {
		trace.Observation = "generation_failed; final answer preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("test generator failed; keeping original answer", "task_id", task.ID, "error", err)
		e.observeTestGenerationUsage(usage)
		return
	}
	if result == nil {
		trace.Observation = "generation_failed; final answer preserved"
		task.Trace = append(task.Trace, trace)
		e.observeTestGenerationUsage(usage)
		return
	}
	trace.Observation = fmt.Sprintf("suggestions=%d summary=%s", len(result.Suggestions), result.Summary)
	for _, suggestion := range result.Suggestions {
		trace.Evidence = append(trace.Evidence, types.Evidence{Path: suggestion.Path, Query: "suggested regression test", Lines: []string{fmt.Sprintf("[%s] %s: covers %s; %s", suggestion.Priority, suggestion.Name, suggestion.Covers, suggestion.Rationale)}})
	}
	task.Trace = append(task.Trace, trace)
	e.observeTestGenerationUsage(usage)
	if len(result.Suggestions) == 0 {
		return
	}
	var section strings.Builder
	section.WriteString("\n\n## Suggested tests\n")
	for _, suggestion := range result.Suggestions {
		fmt.Fprintf(&section, "- [%s] `%s` %s (%s): %s. %s\n\n", strings.ToUpper(suggestion.Priority), suggestion.Path, suggestion.Name, suggestion.Framework, suggestion.Covers, suggestion.Rationale)
		for _, line := range strings.Split(suggestion.SuggestedCode, "\n") {
			section.WriteString("    ")
			section.WriteString(line)
			section.WriteByte('\n')
		}
	}
	task.FinalAnswer = strings.TrimSpace(task.FinalAnswer) + strings.TrimRight(section.String(), "\n")
}

func (e *Engine) observeTestGenerationUsage(usage types.TokenUsage) {
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "test_generator")
	}
}

func (e *Engine) diagnoseFailure(ctx context.Context, task *types.Task, failure error) {
	if e.FailureDiagnoser == nil || !e.llmSceneEnabled(config.LLMSceneFailureDiagnoser) || taskHasAction(task, diagnostics.TraceAction) {
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneFailureDiagnoser, task) {
		return
	}
	result, usage, err := e.FailureDiagnoser.Diagnose(ctx, task, failure)
	trace := types.StepTrace{Step: task.StepCount, Action: diagnostics.TraceAction, TokenUsage: usage}
	if err != nil || result == nil {
		trace.Observation = "diagnosis_failed; original failure preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("failure diagnoser failed; preserving original failure", "task_id", task.ID, "error", err)
		e.observeFailureDiagnosisUsage(usage)
		return
	}
	trace.Query = result.Category
	trace.Observation = fmt.Sprintf("retryable=%t root_cause=%s", result.Retryable, result.RootCause)
	lines := append([]string{"Root cause: " + result.RootCause}, result.Evidence...)
	for index, step := range result.RecoverySteps {
		lines = append(lines, fmt.Sprintf("Recovery %d: %s", index+1, step))
	}
	trace.Evidence = []types.Evidence{{Path: result.FailedAction, Query: "failure diagnosis", Lines: lines}}
	task.Trace = append(task.Trace, trace)
	e.observeFailureDiagnosisUsage(usage)

	var answer strings.Builder
	fmt.Fprintf(&answer, "Failed: %s\n\n## Failure diagnosis\n- Category: %s\n- Root cause: %s\n- Failed step: %d", sanitize.Secrets(failure.Error()), result.Category, result.RootCause, result.FailedStep)
	if result.FailedAction != "" {
		fmt.Fprintf(&answer, "\n- Failed action: `%s`", result.FailedAction)
	}
	fmt.Fprintf(&answer, "\n- Retryable: %t\n\n### Recovery steps\n", result.Retryable)
	for index, step := range result.RecoverySteps {
		fmt.Fprintf(&answer, "%d. %s\n", index+1, step)
	}
	task.FinalAnswer = strings.TrimSpace(answer.String())
}

func (e *Engine) observeFailureDiagnosisUsage(usage types.TokenUsage) {
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "failure_diagnoser")
	}
}

func (e *Engine) critiqueDecision(ctx context.Context, task *types.Task, decision *planner.PlanDecision) {
	if e.PlanCritic == nil || decision == nil || decision.Stop || !e.llmSceneEnabled(config.LLMScenePlanCritic) {
		return
	}
	if !llmcore.AllowedForTask(config.LLMScenePlanCritic, task) {
		return
	}
	plan := plancritic.Plan{Summary: decision.ThoughtSummary, Steps: make([]plancritic.Step, 0, len(decision.Actions))}
	for _, action := range decision.Actions {
		if action.Action == "none" {
			continue
		}
		plan.Steps = append(plan.Steps, plancritic.Step{Action: action.Action, Parameters: action.Parameters})
	}
	if len(plan.Steps) == 0 || !plancritic.ShouldCritique(task, plan) {
		return
	}
	fingerprint := plancritic.Fingerprint(plan)
	if plancritic.AlreadyCritiqued(task, fingerprint) {
		return
	}
	result, usage, err := e.PlanCritic.Critique(ctx, task, plan)
	plancritic.ApplyResult(task, plan, result, usage, err)
	if err != nil {
		engineLog.Warn("plan critic failed; deterministic controls remain active", "task_id", task.ID, "error", err)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "plan_critic")
	}
}

func (e *Engine) collectChanges(ctx context.Context, workspace string) (review.ChangeSet, error) {
	collector := e.CollectCodeChanges
	if collector == nil {
		collector = review.CollectChanges
	}
	return collector(ctx, workspace)
}

func (e *Engine) runCodeQualityGates(ctx context.Context, task *types.Task) {
	if !review.TaskMayHaveCodeChanges(task) || (!e.codeReviewEligible(task) && !e.testGenerationEligible(task)) {
		return
	}
	changes, err := e.collectChanges(ctx, task.Workspace)
	if err != nil {
		engineLog.Warn("code quality change collection failed; skipping gates", "task_id", task.ID, "error", err)
		return
	}
	if strings.TrimSpace(changes.Diff) == "" || len(changes.Paths) == 0 {
		return
	}
	if e.codeReviewEligible(task) {
		e.reviewCodeChangesWithSet(ctx, task, changes)
	}
	// Re-evaluate after code review because it may consume the remaining task
	// token allowance needed by the test generation scene.
	if e.testGenerationEligible(task) {
		e.generateTestSuggestionsWithSet(ctx, task, changes)
	}
}

func (e *Engine) calibrateAnswerUncertainty(ctx context.Context, task *types.Task) {
	if e.AnswerUncertaintyCalibrator == nil || !e.llmSceneEnabled(config.LLMSceneAnswerUncertaintyCalibrator) || !uncertainty.ShouldCalibrate(task) || !llmcore.AllowedForTask(config.LLMSceneAnswerUncertaintyCalibrator, task) {
		return
	}
	result, usage, err := e.AnswerUncertaintyCalibrator.Calibrate(ctx, task, task.FinalAnswer)
	uncertainty.Apply(task, result, usage, err)
	if err != nil {
		engineLog.Warn("answer uncertainty calibrator failed; final answer preserved", "task_id", task.ID, "error", err)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "answer_uncertainty_calibrator")
	}
}

func (e *Engine) checkFactFreshness(ctx context.Context, task *types.Task) {
	if e.FactFreshnessChecker == nil || !e.llmSceneEnabled(config.LLMSceneFactFreshnessChecker) || !factfreshness.ShouldCheck(task) || !llmcore.AllowedForTask(config.LLMSceneFactFreshnessChecker, task) {
		return
	}
	result, usage, err := e.FactFreshnessChecker.Check(ctx, task, task.FinalAnswer)
	factfreshness.Apply(task, result, usage, err)
	if err != nil {
		engineLog.Warn("fact freshness checker failed; no risk marker added", "task_id", task.ID, "error", err)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "fact_freshness_checker")
	}
}

func (e *Engine) checkNumericConsistency(ctx context.Context, task *types.Task) {
	if e.NumericConsistencyChecker == nil || !e.llmSceneEnabled(config.LLMSceneNumericConsistencyChecker) || !numericconsistency.ShouldCheck(task) || !llmcore.AllowedForTask(config.LLMSceneNumericConsistencyChecker, task) {
		return
	}
	result, usage, err := e.NumericConsistencyChecker.Check(ctx, task, task.FinalAnswer)
	numericconsistency.Apply(task, result, usage, err)
	if err != nil {
		engineLog.Warn("numeric consistency checker failed; no risk marker added", "task_id", task.ID, "error", err)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "numeric_consistency_checker")
	}
}

func (e *Engine) safetySceneAvailable(task *types.Task) bool {
	return e.SafetyGuard != nil && e.llmSceneEnabled(config.LLMSceneSafetyGuard) && llmcore.AllowedForTask(config.LLMSceneSafetyGuard, task)
}

func (e *Engine) guardInput(ctx context.Context, task *types.Task) bool {
	if !e.safetySceneAvailable(task) || taskHasAction(task, "safety_guard_input") {
		return true
	}
	decision, usage, err := e.SafetyGuard.Evaluate(ctx, policy.SafetyStageInput, task, task.Goal)
	trace := types.StepTrace{Step: task.StepCount, Action: "safety_guard_input", TokenUsage: usage}
	if err != nil {
		trace.Observation = "check_failed; existing deterministic policies remain active"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("input safety guard failed; continuing with deterministic policies", "task_id", task.ID, "error", err)
		e.observeSafetyUsage(usage, "safety_guard_input")
		return true
	}
	if decision == nil || strings.TrimSpace(decision.SafeText) == "" {
		trace.Observation = "check_failed; existing deterministic policies remain active"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("input safety guard returned no decision; continuing with deterministic policies", "task_id", task.ID)
		e.observeSafetyUsage(usage, "safety_guard_input")
		return true
	}
	trace.Observation = fmt.Sprintf("allowed=%t categories=%s", decision.Allowed, strings.Join(decision.Categories, ","))
	task.Trace = append(task.Trace, trace)
	e.observeSafetyUsage(usage, "safety_guard_input")
	if decision.Allowed {
		return true
	}
	_ = SetTaskCompleted(task, decision.SafeText)
	if e.Metrics != nil {
		e.Metrics.IncCompleted()
	}
	return false
}

func (e *Engine) guardOutput(ctx context.Context, task *types.Task) {
	if !e.safetySceneAvailable(task) {
		return
	}
	decision, usage, err := e.SafetyGuard.Evaluate(ctx, policy.SafetyStageOutput, task, task.FinalAnswer)
	trace := types.StepTrace{Step: task.StepCount, Action: "safety_guard_output", TokenUsage: usage}
	if err != nil {
		trace.Observation = "check_failed; output preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("output safety guard failed; keeping original answer", "task_id", task.ID, "error", err)
		e.observeSafetyUsage(usage, "safety_guard_output")
		return
	}
	if decision == nil || strings.TrimSpace(decision.SafeText) == "" {
		trace.Observation = "check_failed; output preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("output safety guard returned no decision; keeping original answer", "task_id", task.ID)
		e.observeSafetyUsage(usage, "safety_guard_output")
		return
	}
	trace.Observation = fmt.Sprintf("allowed=%t categories=%s", decision.Allowed, strings.Join(decision.Categories, ","))
	task.Trace = append(task.Trace, trace)
	task.FinalAnswer = decision.SafeText
	e.observeSafetyUsage(usage, "safety_guard_output")
}

func (e *Engine) observeSafetyUsage(usage types.TokenUsage, operation string) {
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, operation)
	}
}

func taskHasAction(task *types.Task, action string) bool {
	for _, trace := range task.Trace {
		if trace.Action == action {
			return true
		}
	}
	return false
}

func (e *Engine) routeIntent(ctx context.Context, task *types.Task) {
	if e.IntentRouter == nil || !e.llmSceneEnabled(config.LLMSceneIntentRouter) || taskHasAction(task, llmcore.IntentRouteTraceAction) {
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneIntentRouter, task) {
		return
	}
	decision, usage, err := e.IntentRouter.Route(ctx, task)
	trace := types.StepTrace{Step: task.StepCount, Action: llmcore.IntentRouteTraceAction, TokenUsage: usage}
	if err != nil || decision == nil {
		trace.Observation = "check_failed; default scene routing preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("intent router failed; preserving default scene routing", "task_id", task.ID, "error", err)
		if e.Metrics != nil {
			e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "intent_router")
		}
		return
	}
	details, _ := json.Marshal(map[string]string{"complexity": decision.Complexity, "cost_tier": decision.CostTier, "latency_tier": decision.LatencyTier, "quality_tier": decision.QualityTier})
	trace.Query = decision.Intent
	trace.Observation = string(details)
	task.Trace = append(task.Trace, trace)
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "intent_router")
	}
}

func (e *Engine) ResolveMemoryConflicts(ctx context.Context, task *types.Task) {
	if e.MemoryConflictResolver == nil || !e.llmSceneEnabled(config.LLMSceneMemoryConflictResolver) || len(task.Memories) == 0 {
		return
	}
	evidenceCount := memory.ConflictEvidenceCount(task)
	if len(task.Memories) < 2 && evidenceCount == 0 {
		return
	}
	version := fmt.Sprintf("evidence:%d", evidenceCount)
	for _, trace := range task.Trace {
		if trace.Action == memory.ConflictResolutionTraceAction && trace.Query == version {
			return
		}
	}
	if !llmcore.AllowedForTask(config.LLMSceneMemoryConflictResolver, task) {
		return
	}
	resolution, usage, err := e.MemoryConflictResolver.Resolve(ctx, task)
	trace := types.StepTrace{Step: task.StepCount, Action: memory.ConflictResolutionTraceAction, Query: version, TokenUsage: usage}
	if err != nil || resolution == nil {
		trace.Observation = "check_failed; all retrieved memories preserved"
		task.Trace = append(task.Trace, trace)
		engineLog.Warn("memory conflict resolver failed; preserving memories", "task_id", task.ID, "error", err)
	} else {
		task.Memories = resolution.Memories
		trace.Observation = fmt.Sprintf("kept=%d dropped=%d conflicts=%d", len(resolution.Memories), resolution.Dropped, resolution.ConflictCount)
		task.Trace = append(task.Trace, trace)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "memory_conflict_resolver")
	}
}

var tracer = otel.Tracer("ai-agent/orchestrator")

// engineLog is the package-level logger for engine.go; the package-level "log"
// var is declared in eino.go and shared across the package.
var engineLog = logger.Component("orchestrator")

type Mode string

const (
	ModeEino       Mode = "eino"
	ModeLegacy     Mode = "legacy"
	ModeAdk        Mode = "adk"
	ModeStep       Mode = "step"
	ModeMultiAgent Mode = "multiagent"
)

func (e *Engine) Next(ctx context.Context, task *types.Task) (err error) {
	effectiveMode := e.Mode
	if effectiveMode == "" {
		effectiveMode = ModeEino
	}
	engineLog.Info("running next execution step", "task_id", task.ID, "mode", string(effectiveMode))
	defer func() {
		if types.IsTerminalTaskStatus(task.Status) {
			tools.ClearRetrievalContext(task.ID)
		}
	}()
	wasCompleted := task.Status == types.StatusCompleted
	if types.IsTerminalTaskStatus(task.Status) {
		return nil
	}
	ctx = store.WithTenantScope(ctx, task.TenantID)
	if e.Store != nil {
		if ledger, ok := e.Store.(types.TenantUsageLedger); ok {
			ctx = llmcore.WithTenantUsageLedger(ctx, ledger)
		}
	}
	ctx = llmcore.WithTaskBudget(ctx, task)
	if pipelineCfg := config.Get().AnswerPipeline; e.AnswerPipeline != nil && pipelineCfg.Enabled {
		ctx = llmcore.WithAnswerAuditReserve(ctx, pipelineCfg.AuditTokenReserve)
	}
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	defer func() {
		if e.AnswerPipeline == nil || strings.TrimSpace(task.FinalAnswer) == "" || !types.IsTerminalTaskStatus(task.Status) {
			return
		}
		if task.Status == types.StatusCompleted {
			e.runCodeQualityGates(ctx, task)
		}
		if _, pipelineErr := e.AnswerPipeline.Process(ctx, task, string(effectiveMode)); pipelineErr != nil {
			engineLog.Warn("answer pipeline failed; execution outcome preserved", "task_id", task.ID, "error", pipelineErr)
		}
	}()
	defer func() {
		if err != nil {
			var budgetErr *llmcore.TaskBudgetError
			if errors.As(err, &budgetErr) {
				engineLog.Info("task reached LLM budget", "task_id", task.ID, "kind", budgetErr.Kind, "current", budgetErr.Current, "limit", budgetErr.Limit)
				_ = SetTaskPartial(task, finalAnswerForLimit(task, limitReasonLLMBudget), limitReasonLLMBudget)
				if e.Metrics != nil {
					e.Metrics.IncCompleted()
				}
				err = nil
				return
			}
			safeFailure := sanitize.Secrets(err.Error())
			engineLog.Error("step execution failed", "task_id", task.ID, "error", safeFailure)
			_ = SetTaskFailed(task, safeFailure)
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				e.diagnoseFailure(ctx, task, errors.New(safeFailure))
			}
		}
	}()
	if !wasCompleted && !e.guardInput(ctx, task) {
		return nil
	}
	if !wasCompleted {
		e.routeIntent(ctx, task)
		ctx = llmcore.WithTaskRoutingHints(ctx, task)
	}

	prefetchContext := strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "prefetch")
	if prefetchContext && task.StepCount == 0 && len(task.Memories) == 0 {
		var retrievedMems []types.Memory
		retrievalQuery := task.Goal
		var ragUsage types.TokenUsage
		if llmcore.AllowedForTask(config.LLMSceneRAGQueryRewriter, task) {
			retrievalQuery, ragUsage = memory.RewriteRAGQuery(ctx, task.Goal)
		}

		// 1. Try querying third-party RAG search URL if configured (query up to 5 candidates)
		if config.Get().RAG.SearchURL != "" {
			if extMems, extErr := memory.SearchThirdPartyRAG(ctx, retrievalQuery); extErr == nil && len(extMems) > 0 {
				extMems = e.inspectExternalMemories(ctx, task, extMems)
				retrievedMems = append(retrievedMems, extMems...)
				engineLog.Info("retrieved memories from third-party RAG URL", "task_id", task.ID, "count", len(extMems))
			} else if extErr != nil {
				engineLog.Warn("failed to query third-party RAG URL", "error", extErr)
			}
		}

		// 2. Query local Store for up to 5 candidates
		if e.Store != nil {
			engineLog.Info("querying local long-term memory", "task_id", task.ID, "goal", task.Goal)
			if emb, embErr := memory.GetEmbedding(ctx, retrievalQuery); embErr == nil {
				if mems, queryErr := e.Store.QueryMemories(ctx, retrievalQuery, emb, 5); queryErr == nil && len(mems) > 0 {
					for _, m := range mems {
						retrievedMems = append(retrievedMems, *m)
					}
					engineLog.Info("retrieved relevant local historical memories", "task_id", task.ID, "count", len(mems))
				}
			}
		}

		// 3. Deduplicate and limit to top 3 unique memories
		if len(retrievedMems) > 0 {
			deduped := memory.DeduplicateMemories(retrievedMems)
			var rerankUsage types.TokenUsage
			if llmcore.AllowedForTask(config.LLMSceneRAGReranker, task) {
				deduped, rerankUsage = memory.RerankMemories(ctx, retrievalQuery, deduped)
			}
			ragUsage.PromptTokens += rerankUsage.PromptTokens
			ragUsage.CompletionTokens += rerankUsage.CompletionTokens
			ragUsage.TotalTokens += rerankUsage.TotalTokens
			if len(deduped) > 3 {
				deduped = deduped[:3]
			}
			task.Memories = deduped
			engineLog.Info("final RAG memories after deduplication", "task_id", task.ID, "count", len(task.Memories))
		}
		if ragUsage.TotalTokens > 0 {
			task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: "rag_prepare", Observation: "LLM-assisted query preparation and memory ranking", TokenUsage: ragUsage})
			if e.Metrics != nil {
				e.Metrics.ObserveTokens(ragUsage.PromptTokens, ragUsage.CompletionTokens, ragUsage.TotalTokens, "rag")
			}
		}
	}
	if !wasCompleted && prefetchContext {
		e.ResolveMemoryConflicts(ctx, task)
		ctx = llmcore.WithTaskRoutingHints(ctx, task)
	}

	switch e.Mode {
	case "", ModeEino:
		err = e.runEinoNext(ctx, task)
	case ModeLegacy:
		err = e.runLegacyNext(ctx, task)
	case ModeAdk:
		err = e.runAdkNext(ctx, task)
	case ModeStep:
		err = e.runStepNext(ctx, task)
	case ModeMultiAgent:
		err = e.runMultiAgentNext(ctx, task)
	default:
		err = fmt.Errorf("unsupported orchestrator mode: %s", e.Mode)
	}
	if e.AnswerPipeline == nil && err == nil && !wasCompleted && task.Status == types.StatusCompleted && strings.TrimSpace(task.FinalAnswer) != "" {
		e.verifyCitations(ctx, task)
		e.runCodeQualityGates(ctx, task)
		e.checkFactFreshness(ctx, task)
		e.checkNumericConsistency(ctx, task)
		e.calibrateAnswerUncertainty(ctx, task)
		e.guardOutput(ctx, task)
	}
	return err
}

func (e *Engine) runLegacyNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	engineLog.Info("running step", "step", task.StepCount+1, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "task_id", task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "legacy"),
	)

	totalTokens := 0
	for _, tr := range task.Trace {
		totalTokens += tr.TokenUsage.TotalTokens
	}

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 || (task.TokenBudget > 0 && totalTokens >= task.TokenBudget) {
		engineLog.Info("task reached limit", "task_id", task.ID, "step", task.StepCount, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "tokens", totalTokens, "token_budget", task.TokenBudget)
		if task.TokenBudget > 0 && totalTokens >= task.TokenBudget {
			_ = SetTaskPartial(task, finalAnswerForLimit(task, limitReasonTokenBudget), limitReasonTokenBudget)
		} else {
			e.completeAtExecutionLimit(ctx, task)
		}
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "budget_or_max_steps"))
		return nil
	}

	pStart := time.Now()
	decision, err := e.Planner.PlanNext(ctx, task, func(chunk string) {
		if e.TokenCallback != nil {
			e.TokenCallback(task.ID, chunk)
		}
	})
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		engineLog.Error("planner failed", "task_id", task.ID, "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner failure")
		return err
	}
	if e.finalizeBeforeRetrievalExpansion(ctx, task, decision) {
		return nil
	}
	e.critiqueDecision(ctx, task, decision)

	if e.Metrics != nil {
		e.Metrics.ObserveTokens(
			decision.TokenUsage.PromptTokens,
			decision.TokenUsage.CompletionTokens,
			decision.TokenUsage.TotalTokens,
			"planner",
		)
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}

	engineLog.Info("planner decided", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames)

	task.Hypothesis = decision.ThoughtSummary
	span.SetAttributes(
		attribute.StringSlice("agent.planner.actions", actionNames),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	if decision.Stop {
		engineLog.Info("planner decided to stop", "task_id", task.ID, "final_answer", decision.FinalAnswer)
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount + 1,
			Action:      "stop",
			Observation: decision.FinalAnswer,
			TokenUsage:  decision.TokenUsage,
		})
		_ = SetTaskCompleted(task, decision.FinalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "planner_stop"))
		return nil
	}

	rejected, err := e.enforceApprovals(ctx, task, decision)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "approval failed")
		return err
	}

	if rejected {
		// Action rejected by user. The rejection trace has already been appended
		// in SuspendForApproval. We skip executing the tools and return nil to
		// allow the planner to adapt in the next cycle.
		return nil
	}

	engineLog.Info("executing actions", "task_id", task.ID, "actions", actionNames)
	xStart := time.Now()
	traces, err := e.Executor.Execute(ctx, task, decision)
	traces, injectionAudit := e.inspectExternalTraces(ctx, task, traces)
	traces, relevanceAudits := e.filterExternalTraces(ctx, task, traces)
	conflictAudits := e.resolveEvidenceConflicts(ctx, task, traces)

	// Tool failures are recorded in the traces and are non-fatal (the executor
	// only returns err on context cancellation); surface them to metrics but
	// keep the task running so the planner can observe and recover next turn.
	failed := countFailedTraces(traces)
	obsErr := err
	if obsErr == nil && failed > 0 {
		obsErr = fmt.Errorf("%d of %d actions failed", failed, len(traces))
	}
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), obsErr, "batch")
	}
	if err != nil {
		engineLog.Error("executor aborted", "task_id", task.ID, "actions", actionNames, "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "executor aborted")
		return err
	}

	if failed > 0 {
		engineLog.Warn("some actions failed; recorded as observations, continuing", "task_id", task.ID, "failed", failed, "total", len(traces))
		span.SetAttributes(attribute.Int("agent.executor.failed_actions", failed))
	} else {
		engineLog.Info("action execution success", "task_id", task.ID, "traces", len(traces))
	}

	task.StepCount += len(traces)
	task.ToolBudget -= len(traces)
	for i := range traces {
		if i == 0 {
			traces[i].TokenUsage.PromptTokens += decision.TokenUsage.PromptTokens
			traces[i].TokenUsage.CompletionTokens += decision.TokenUsage.CompletionTokens
			traces[i].TokenUsage.TotalTokens += decision.TokenUsage.TotalTokens
		}
	}
	task.Trace = append(task.Trace, traces...)
	if injectionAudit != nil {
		task.Trace = append(task.Trace, *injectionAudit)
	}
	task.Trace = append(task.Trace, relevanceAudits...)
	task.Trace = append(task.Trace, conflictAudits...)
	_ = SetTaskRunning(task)

	engineLog.Info("step completed", "step", task.StepCount, "task_id", task.ID, "remaining_budget", task.ToolBudget)

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

// enforceApprovals gates every RiskLevelHigh action in the decision behind the
// human approval flow before any tool runs. It returns rejected=true if the user
// rejected an action (the rejection trace is appended by SuspendForApproval, so
// the caller should skip execution and let the planner adapt next cycle); err is
// non-nil only on approval-flow failure (e.g. context cancellation).
//
// This is the single source of truth for high-risk gating across orchestrator
// modes. It MUST be called before Executor.Execute in every mode that runs an
// LLM-chosen PlanDecision — previously the gate lived only in runLegacyNext, so
// the default eino mode (and step/adk) executed write_file/execute_code with no
// approval at all (see BUG_REPORT.md #1).
func (e *Engine) enforceApprovals(ctx context.Context, task *types.Task, decision *planner.PlanDecision) (rejected bool, err error) {
	for i := range decision.Actions {
		ac := &decision.Actions[i]
		tool, ok := tools.Get(ac.Action)
		if !ok || tool.RiskLevel() != types.RiskLevelHigh {
			continue
		}
		approved, newParams, apErr := e.SuspendForApproval(ctx, task, ac.Action, ac.Parameters)
		if apErr != nil {
			engineLog.Error("action approval error", "task_id", task.ID, "action", ac.Action, "error", apErr)
			return false, apErr
		}
		if !approved {
			return true, nil
		}
		if newParams != nil {
			ac.Parameters = newParams
		}
	}
	return false, nil
}

// approvalStore returns the ApprovalStore this engine should use. When the
// Approvals field is set (e.g. in tests) that instance is used; otherwise the
// process-wide defaultApprovals singleton is returned.
func (e *Engine) approvalStore() *ApprovalStore {
	if e.Approvals != nil {
		return e.Approvals
	}
	return defaultApprovals
}

func (e *Engine) SuspendForApproval(ctx context.Context, task *types.Task, action string, params map[string]any) (bool, map[string]any, error) {
	approval := e.BuildApprovalRequest(task, action, params)
	store := e.approvalStore()
	approvalID, ch := store.Register(task.ID, approval)
	defer store.Remove(approvalID)

	task.Status = types.StatusAwaitingApproval
	if e.Store != nil {
		// Persistence must survive caller ctx expiry: the caller's ctx carries
		// the run-all wall-clock budget and the user's approval-wait window,
		// neither of which should be allowed to abort the awaiting_approval
		// write. Without this, a task could observe ctx.Done() below and leave
		// the DB row in a pre-suspend state, looking lost across restarts.
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := e.Store.SaveFullTask(saveCtx, task)
		cancel()
		if err != nil {
			return false, nil, err
		}
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusAwaitingApproval)
	}
	if e.ApprovalCallback != nil {
		e.ApprovalCallback(task.ID, approval)
	}

	var res types.ApprovalResult
	select {
	case res = <-ch:
	case <-ctx.Done():
		select {
		case res = <-ch:
		default:
			return false, nil, ctx.Err()
		}
	}

	if !res.Approved {
		msg := res.Message
		if msg == "" {
			msg = "No reason provided"
		}
		role := types.AgentRoleSingle
		if e.Mode == ModeMultiAgent {
			role = types.AgentRoleResearcher
		}
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount + 1,
			Goal:        task.Goal,
			Action:      action,
			Observation: fmt.Sprintf("Action rejected by user. Reason: %s", msg),
			Error:       fmt.Sprintf("Action %s rejected by user: %s", action, msg),
			Evidence: []types.Evidence{{
				Path:  "user_feedback",
				Lines: []string{msg},
				Query: "disapproval",
			}},
			AgentRole: role,
		})
		task.StepCount += 1

		task.Status = types.StatusRunning
		if e.Store != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = e.Store.SaveFullTask(saveCtx, task)
			cancel()
		}
		if e.EventCallback != nil {
			e.EventCallback(task.ID, types.StatusRunning)
		}
		return false, nil, nil
	}

	task.Status = types.StatusRunning
	if e.Store != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = e.Store.SaveFullTask(saveCtx, task)
		cancel()
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusRunning)
	}
	return true, res.Parameters, nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	engineLog.Info("starting task to completion", "task_id", task.ID, "goal", task.Goal)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	if e.Metrics != nil {
		e.Metrics.IncRunAll()
	}

	for !types.IsTerminalTaskStatus(task.Status) {
		select {
		case <-ctx.Done():
			engineLog.Warn("task canceled", "task_id", task.ID, "error", ctx.Err())
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			_ = SetTaskFailed(task, "task canceled: "+ctx.Err().Error())
			return ctx.Err()
		default:
		}

		traceStart := len(task.Trace)
		if err := e.Next(ctx, task); err != nil {
			engineLog.Error("execution step failed", "task_id", task.ID, "error", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "run_all failed")
			return err
		}

		if e.Store != nil {
			if err := e.Store.SaveFullTask(ctx, task); err != nil {
				engineLog.Error("failed to persist run-all progress", "task_id", task.ID, "error", err)
				span.RecordError(err)
				span.SetStatus(codes.Error, "persist progress failed")
				return err
			}
		}

		if e.StepCallback != nil {
			for i := traceStart; i < len(task.Trace); i++ {
				step := task.Trace[i]
				// A step callback reports execution progress. Task completion is
				// published separately with the authoritative final answer after
				// persistence; marking the last trace terminal can make SSE clients
				// close before they receive that final event.
				e.StepCallback(task.ID, types.StatusRunning, &step)
			}
		}
	}

	engineLog.Info("task finished", "task_id", task.ID, "status", string(task.Status), "final_answer", task.FinalAnswer)
	span.SetAttributes(attribute.String("agent.task.final_answer", task.FinalAnswer))
	return nil
}

func stepFindTextFiles(ctx context.Context, task *types.Task) error {
	engineLog.Info("legacy static path - finding text files", "task_id", task.ID)
	task.Hypothesis = "Relevant evidence is likely inside text or markdown files"

	txtFiles, err := tools.FindFiles(ctx, task.Workspace, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.txt) failed", "error", err)
		return err
	}
	mdFiles, err := tools.FindFiles(ctx, task.Workspace, "*.md")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.md) failed", "error", err)
		return err
	}

	files := append(txtFiles, mdFiles...)
	if len(files) > 20 {
		files = files[:20]
	}

	engineLog.Info("legacy static path - found text/markdown files", "task_id", task.ID, "count", len(files))

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "find_files",
		Query:       "*.txt, *.md",
		Observation: fmt.Sprintf("found %d candidate files", len(files)),
	})

	if len(files) == 0 {
		task.Unresolved = append(task.Unresolved, "no candidate text or markdown files found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepSearchKeyword(ctx context.Context, task *types.Task) error {
	query, err := lastWord(task.Goal)
	if err != nil {
		engineLog.Error("legacy static path - failed to extract keyword", "error", err)
		return err
	}
	engineLog.Info("legacy static path - searching keyword", "keyword", query, "task_id", task.ID)
	task.Hypothesis = "Search the most likely keyword in candidate text files"

	evidence, _, err := tools.SearchWithRG(ctx, task.Workspace, query, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - SearchWithRG failed", "error", err)
		return err
	}

	engineLog.Info("legacy static path - found evidence items", "task_id", task.ID, "count", len(evidence))

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "search_text",
		Query:       query,
		Observation: fmt.Sprintf("found %d evidence items", len(evidence)),
		Evidence:    evidence,
	})

	if len(evidence) == 0 {
		task.Unresolved = append(task.Unresolved, "keyword not found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepReadBestFile(ctx context.Context, task *types.Task) error {
	engineLog.Info("legacy static path - reading best file", "task_id", task.ID)
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) == 0 {
		engineLog.Info("legacy static path - not enough evidence to select a file", "task_id", task.ID)
		_ = SetTaskCompleted(task, "not enough evidence to select a file")
		return nil
	}

	target := task.Trace[1].Evidence[0].Path
	engineLog.Info("legacy static path - target best file identified", "task_id", task.ID, "file", target)
	content, err := tools.ReadFile(task.Workspace, target)
	if err != nil {
		engineLog.Error("legacy static path - ReadFile failed", "error", err)
		return err
	}

	snippet := content
	if len(snippet) > 220 {
		snippet = snippet[:220]
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "read_file",
		Query:       target,
		Observation: "read file snippet: " + snippet,
	})

	_ = SetTaskCompleted(task, fmt.Sprintf("completed search; best candidate file: %s", target))
	return nil
}

func lastWord(s string) (string, error) {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty input, cannot extract keyword")
	}
	return parts[len(parts)-1], nil
}
