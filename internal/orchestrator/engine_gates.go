package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/diagnostics"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

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
