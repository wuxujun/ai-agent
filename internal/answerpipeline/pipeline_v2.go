package answerpipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

const (
	citationStage = "citation_verify"
	safetyStage   = "safety_guard_output"
)

type checkerOutcome[T any] struct {
	stage    types.AnswerAuditStage
	result   *T
	usage    types.TokenUsage
	err      error
	executed bool
	lease    *tokenLease
}

func (p *DefaultPipeline) processV2(ctx context.Context, task *types.Task, mode string) (*types.AnswerAuditReport, error) {
	if task == nil {
		return nil, fmt.Errorf("answer pipeline requires task")
	}
	runtimeConfig := config.Get()
	cfg := runtimeConfig.AnswerPipeline
	if !cfg.Enabled {
		return nil, nil
	}
	if tenant, ok := runtimeConfig.API.Tenants[task.TenantID]; ok {
		if enforcement := strings.TrimSpace(tenant.AnswerPipelineEnforcement); enforcement != "" {
			cfg.Enforcement = enforcement
		}
		if len(tenant.AnswerPipelineRequiredStages) > 0 {
			cfg.RequiredStages = append([]string(nil), tenant.AnswerPipelineRequiredStages...)
		}
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	prior := task.AnswerAudit
	report := &types.AnswerAuditReport{
		PipelineVersion: Version,
		DraftHash:       digest(task.FinalAnswer),
		EvidenceHash:    evidenceDigest(task),
		StartedAt:       now,
		Enforcement:     normalizedEnforcement(cfg.Enforcement),
		Publishable:     strings.TrimSpace(task.FinalAnswer) != "",
	}

	if hasBlockedInput(task) {
		for _, spec := range []struct{ name, status string }{
			{citationStage, "not_applicable"},
			{factfreshness.TraceAction, "not_applicable"},
			{numericconsistency.TraceAction, "not_applicable"},
			{uncertainty.TraceAction, "not_applicable"},
			{safetyStage, "passed"},
		} {
			fp := stageFingerprint(spec.name, task.FinalAnswer, report.EvidenceHash, referenceDate(spec.name, now))
			report.Stages = append(report.Stages, v2Stage(spec.name, spec.status, "covered_by_input_guard", types.TokenUsage{}, nil, time.Time{}, fp))
		}
		finishV2(report, task, cfg.RequiredStages, cfg.OnRequiredStageFailure)
		p.observeReport(mode, report)
		return report, nil
	}

	p.importVerifierFindings(task, report)
	budget := newAuditBudget(task, cfg.AuditTokenReserve)
	var safetyLease *tokenLease
	safetyLeaseDenied := false
	if p.SafetyGuard != nil && p.enabled(config.LLMSceneSafetyGuard) && llmcore.AllowedForTask(config.LLMSceneSafetyGuard, task) {
		var ok bool
		safetyLease, ok = budget.reserveTokens(stageTokenBudget(cfg.StageTokenBudgets, safetyStage))
		safetyLeaseDenied = !ok
	}
	protectedAuditTokens := p.protectedAuditTokens(task, cfg.StageTokenBudgets)
	p.runCitationV2(ctx, task, report, prior, budget, cfg.StageTokenBudgets, protectedAuditTokens, cfg.StageTimeoutSeconds, now)
	p.runEvidenceAuditsV2(ctx, task, report, prior, budget, cfg.StageTokenBudgets, cfg.StageTimeoutSeconds, cfg.ParallelAudits && !llmcore.TaskCostBudgetEnabled(ctx), now)
	p.runUncertaintyV2(ctx, task, report, prior, budget, cfg.StageTokenBudgets, cfg.StageTimeoutSeconds, now)
	p.runSafetyV2(ctx, task, report, prior, safetyLease, safetyLeaseDenied, cfg.StageTokenBudgets, cfg.StageTimeoutSeconds, now)
	finishV2(report, task, cfg.RequiredStages, cfg.OnRequiredStageFailure)
	p.observeReport(mode, report)
	return report, nil
}

func (p *DefaultPipeline) observeReport(mode string, report *types.AnswerAuditReport) {
	if p.ObserveReport != nil {
		p.ObserveReport(mode, report)
	}
}

func (p *DefaultPipeline) runCitationV2(ctx context.Context, task *types.Task, report *types.AnswerAuditReport, prior *types.AnswerAuditReport, budget *auditBudget, budgets map[string]int, protectedTokens, timeoutSeconds int, now time.Time) {
	started := time.Now()
	fp := stageFingerprint(citationStage, task.FinalAnswer, report.EvidenceHash, "")
	if cached, ok := reusableStage(prior, citationStage, fp); ok {
		report.Stages = append(report.Stages, cached)
		return
	}
	if p.CitationVerifier == nil || !p.enabled(config.LLMSceneCitationVerifier) {
		report.Stages = append(report.Stages, v2Stage(citationStage, "disabled", "disabled", types.TokenUsage{}, nil, started, fp))
		return
	}
	if !planner.HasCitationEvidence(task) {
		report.Stages = append(report.Stages, v2Stage(citationStage, "not_applicable", "eligibility", types.TokenUsage{}, nil, started, fp))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneCitationVerifier, task) {
		report.Stages = append(report.Stages, v2Stage(citationStage, "budget_insufficient", "token_gate", types.TokenUsage{}, nil, started, fp))
		return
	}
	lease, ok := budget.reserveTokensKeeping(stageTokenBudget(budgets, citationStage), protectedTokens)
	if !ok {
		report.Stages = append(report.Stages, v2Stage(citationStage, "budget_insufficient", "audit_budget_lease_denied", types.TokenUsage{}, nil, started, fp))
		return
	}
	callCtx, cancel := stageContext(ctx, timeoutSeconds)
	result, usage, err := safeCitationCall(callCtx, p.CitationVerifier, task, task.FinalAnswer)
	cancel()
	lease.commit(usage.TotalTokens)
	p.observe(usage, "citation_verifier")
	if err != nil || result == nil || strings.TrimSpace(result.VerifiedAnswer) == "" {
		status := failureStatus(err)
		report.Stages = append(report.Stages, v2Stage(citationStage, status, failureReason(err), usage, nil, started, fp))
		return
	}
	task.FinalAnswer = result.VerifiedAnswer
	task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: citationStage, Observation: fmt.Sprintf("supported=%t unsupported_claims=%d citation_issues=%d", result.Supported, len(result.UnsupportedClaims), len(result.CitationIssues)), TokenUsage: usage})
	status := "passed"
	if !result.Supported {
		status = "warned"
	}
	report.Stages = append(report.Stages, v2Stage(citationStage, status, "", usage, nil, started, fp))
	_ = now
}

func (p *DefaultPipeline) protectedAuditTokens(task *types.Task, budgets map[string]int) int {
	snapshot := auditSnapshot(task)
	total := 0
	if p.FreshnessChecker != nil && p.enabled(config.LLMSceneFactFreshnessChecker) && factfreshness.ShouldCheck(snapshot) {
		total += stageTokenBudget(budgets, factfreshness.TraceAction)
	}
	if p.NumericChecker != nil && p.enabled(config.LLMSceneNumericConsistencyChecker) && numericconsistency.ShouldCheck(snapshot) {
		total += stageTokenBudget(budgets, numericconsistency.TraceAction)
	}
	if p.UncertaintyCalibrator != nil && p.enabled(config.LLMSceneAnswerUncertaintyCalibrator) && uncertainty.ShouldCalibrate(snapshot) {
		total += stageTokenBudget(budgets, uncertainty.TraceAction)
	}
	return total
}

func (p *DefaultPipeline) runEvidenceAuditsV2(ctx context.Context, task *types.Task, report *types.AnswerAuditReport, prior *types.AnswerAuditReport, budget *auditBudget, budgets map[string]int, timeoutSeconds int, parallel bool, now time.Time) {
	base := auditSnapshot(task)
	fresh := p.prepareFreshness(base, task.FinalAnswer, report.EvidenceHash, prior, budget, budgets, now)
	numeric := p.prepareNumeric(base, task.FinalAnswer, report.EvidenceHash, prior, budget, budgets)

	if parallel && fresh.executed && numeric.executed {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); p.executeFreshness(ctx, base, task.FinalAnswer, timeoutSeconds, &fresh) }()
		go func() { defer wg.Done(); p.executeNumeric(ctx, base, task.FinalAnswer, timeoutSeconds, &numeric) }()
		wg.Wait()
	} else {
		if fresh.executed {
			p.executeFreshness(ctx, base, task.FinalAnswer, timeoutSeconds, &fresh)
		}
		if numeric.executed {
			p.executeNumeric(ctx, base, task.FinalAnswer, timeoutSeconds, &numeric)
		}
	}

	p.applyFreshness(task, base, report, fresh)
	p.applyNumeric(task, base, report, numeric)
}

func (p *DefaultPipeline) prepareFreshness(task *types.Task, answer, evidenceHash string, prior *types.AnswerAuditReport, budget *auditBudget, budgets map[string]int, now time.Time) checkerOutcome[factfreshness.Result] {
	started := time.Now()
	fp := stageFingerprint(factfreshness.TraceAction, answer, evidenceHash, referenceDate(factfreshness.TraceAction, now))
	if cached, ok := reusableStage(prior, factfreshness.TraceAction, fp); ok {
		return checkerOutcome[factfreshness.Result]{stage: cached}
	}
	if p.FreshnessChecker == nil || !p.enabled(config.LLMSceneFactFreshnessChecker) {
		return checkerOutcome[factfreshness.Result]{stage: v2Stage(factfreshness.TraceAction, "disabled", "disabled", types.TokenUsage{}, nil, started, fp)}
	}
	if !factfreshness.ShouldCheck(task) {
		return checkerOutcome[factfreshness.Result]{stage: v2Stage(factfreshness.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started, fp)}
	}
	if !llmcore.AllowedForTask(config.LLMSceneFactFreshnessChecker, task) {
		return checkerOutcome[factfreshness.Result]{stage: v2Stage(factfreshness.TraceAction, "budget_insufficient", "token_gate", types.TokenUsage{}, nil, started, fp)}
	}
	lease, ok := budget.reserveTokens(stageTokenBudget(budgets, factfreshness.TraceAction))
	if !ok {
		return checkerOutcome[factfreshness.Result]{stage: v2Stage(factfreshness.TraceAction, "budget_insufficient", "audit_budget_lease_denied", types.TokenUsage{}, nil, started, fp)}
	}
	return checkerOutcome[factfreshness.Result]{stage: v2Stage(factfreshness.TraceAction, "", "", types.TokenUsage{}, nil, started, fp), executed: true, lease: lease}
}

func (p *DefaultPipeline) prepareNumeric(task *types.Task, answer, evidenceHash string, prior *types.AnswerAuditReport, budget *auditBudget, budgets map[string]int) checkerOutcome[numericconsistency.Result] {
	started := time.Now()
	fp := stageFingerprint(numericconsistency.TraceAction, answer, evidenceHash, "")
	if cached, ok := reusableStage(prior, numericconsistency.TraceAction, fp); ok {
		return checkerOutcome[numericconsistency.Result]{stage: cached}
	}
	if p.NumericChecker == nil || !p.enabled(config.LLMSceneNumericConsistencyChecker) {
		return checkerOutcome[numericconsistency.Result]{stage: v2Stage(numericconsistency.TraceAction, "disabled", "disabled", types.TokenUsage{}, nil, started, fp)}
	}
	if !numericconsistency.ShouldCheck(task) {
		return checkerOutcome[numericconsistency.Result]{stage: v2Stage(numericconsistency.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started, fp)}
	}
	if !llmcore.AllowedForTask(config.LLMSceneNumericConsistencyChecker, task) {
		return checkerOutcome[numericconsistency.Result]{stage: v2Stage(numericconsistency.TraceAction, "budget_insufficient", "token_gate", types.TokenUsage{}, nil, started, fp)}
	}
	lease, ok := budget.reserveTokens(stageTokenBudget(budgets, numericconsistency.TraceAction))
	if !ok {
		return checkerOutcome[numericconsistency.Result]{stage: v2Stage(numericconsistency.TraceAction, "budget_insufficient", "audit_budget_lease_denied", types.TokenUsage{}, nil, started, fp)}
	}
	return checkerOutcome[numericconsistency.Result]{stage: v2Stage(numericconsistency.TraceAction, "", "", types.TokenUsage{}, nil, started, fp), executed: true, lease: lease}
}

func (p *DefaultPipeline) executeFreshness(ctx context.Context, task *types.Task, answer string, timeoutSeconds int, out *checkerOutcome[factfreshness.Result]) {
	callCtx, cancel := stageContext(ctx, timeoutSeconds)
	out.result, out.usage, out.err = safeFreshnessCall(callCtx, p.FreshnessChecker, task, answer)
	cancel()
	out.lease.commit(out.usage.TotalTokens)
	out.stage.TokenUsage = out.usage
	out.stage.Status = resultStatusFreshness(out.result, out.err)
	out.stage.Reason = failureReason(out.err)
	out.stage.Findings = freshnessFindings(out.result)
}

func (p *DefaultPipeline) executeNumeric(ctx context.Context, task *types.Task, answer string, timeoutSeconds int, out *checkerOutcome[numericconsistency.Result]) {
	callCtx, cancel := stageContext(ctx, timeoutSeconds)
	out.result, out.usage, out.err = safeNumericCall(callCtx, p.NumericChecker, task, answer)
	cancel()
	out.lease.commit(out.usage.TotalTokens)
	out.stage.TokenUsage = out.usage
	out.stage.Status = resultStatusNumeric(out.result, out.err)
	out.stage.Reason = failureReason(out.err)
	out.stage.Findings = numericFindings(out.result)
}

func (p *DefaultPipeline) applyFreshness(task, input *types.Task, report *types.AnswerAuditReport, out checkerOutcome[factfreshness.Result]) {
	if out.executed {
		applied := types.CloneTask(input)
		factfreshness.Apply(applied, out.result, out.usage, out.err)
		if len(applied.Trace) > len(input.Trace) {
			trace := applied.Trace[len(applied.Trace)-1]
			trace.Step = task.StepCount
			task.Trace = append(task.Trace, trace)
		}
		p.observe(out.usage, "fact_freshness_checker")
	}
	report.Stages = append(report.Stages, out.stage)
}

func (p *DefaultPipeline) applyNumeric(task, input *types.Task, report *types.AnswerAuditReport, out checkerOutcome[numericconsistency.Result]) {
	if out.executed {
		applied := types.CloneTask(input)
		numericconsistency.Apply(applied, out.result, out.usage, out.err)
		if len(applied.Trace) > len(input.Trace) {
			trace := applied.Trace[len(applied.Trace)-1]
			trace.Step = task.StepCount
			task.Trace = append(task.Trace, trace)
		}
		p.observe(out.usage, "numeric_consistency_checker")
	}
	report.Stages = append(report.Stages, out.stage)
}

func (p *DefaultPipeline) runUncertaintyV2(ctx context.Context, task *types.Task, report *types.AnswerAuditReport, prior *types.AnswerAuditReport, budget *auditBudget, budgets map[string]int, timeoutSeconds int, now time.Time) {
	started := time.Now()
	fp := stageFingerprint(uncertainty.TraceAction, task.FinalAnswer, report.EvidenceHash, "")
	if cached, ok := reusableStage(prior, uncertainty.TraceAction, fp); ok {
		report.Stages = append(report.Stages, cached)
		report.FinalConfidence = prior.FinalConfidence
		return
	}
	if dependencyFailed(report, factfreshness.TraceAction, numericconsistency.TraceAction) {
		report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, "dependency_failed", "dependency_failed", types.TokenUsage{}, nil, started, fp))
		return
	}
	if p.UncertaintyCalibrator == nil || !p.enabled(config.LLMSceneAnswerUncertaintyCalibrator) {
		report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, "disabled", "disabled", types.TokenUsage{}, nil, started, fp))
		return
	}
	snapshot := auditSnapshotKeeping(task, factfreshness.TraceAction, numericconsistency.TraceAction)
	if !uncertainty.ShouldCalibrate(snapshot) {
		report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started, fp))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneAnswerUncertaintyCalibrator, task) {
		report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, "budget_insufficient", "token_gate", types.TokenUsage{}, nil, started, fp))
		return
	}
	lease, ok := budget.reserveTokens(stageTokenBudget(budgets, uncertainty.TraceAction))
	if !ok {
		report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, "budget_insufficient", "audit_budget_lease_denied", types.TokenUsage{}, nil, started, fp))
		return
	}
	callCtx, cancel := stageContext(ctx, timeoutSeconds)
	result, usage, err := safeUncertaintyCall(callCtx, p.UncertaintyCalibrator, snapshot, task.FinalAnswer)
	cancel()
	lease.commit(usage.TotalTokens)
	uncertainty.Apply(task, result, usage, err)
	p.observe(usage, "answer_uncertainty_calibrator")
	status := failureStatus(err)
	if err == nil && result != nil {
		status = "passed"
		report.FinalConfidence = result.Confidence
		if result.NeedsQualification {
			status = "warned"
		}
	}
	report.Stages = append(report.Stages, v2Stage(uncertainty.TraceAction, status, failureReason(err), usage, uncertaintyFindings(result), started, fp))
	_ = now
}

func (p *DefaultPipeline) runSafetyV2(ctx context.Context, task *types.Task, report *types.AnswerAuditReport, prior *types.AnswerAuditReport, lease *tokenLease, leaseDenied bool, budgets map[string]int, timeoutSeconds int, now time.Time) {
	started := time.Now()
	fp := stageFingerprint(safetyStage, task.FinalAnswer, report.EvidenceHash, "")
	if cached, ok := reusableStage(prior, safetyStage, fp); ok {
		lease.commit(0)
		report.Stages = append(report.Stages, cached)
		return
	}
	if p.SafetyGuard == nil || !p.enabled(config.LLMSceneSafetyGuard) {
		lease.commit(0)
		report.Stages = append(report.Stages, v2Stage(safetyStage, "disabled", "disabled", types.TokenUsage{}, nil, started, fp))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneSafetyGuard, task) {
		lease.commit(0)
		report.Stages = append(report.Stages, v2Stage(safetyStage, "budget_insufficient", "token_gate", types.TokenUsage{}, nil, started, fp))
		return
	}
	if leaseDenied {
		report.Stages = append(report.Stages, v2Stage(safetyStage, "budget_insufficient", "audit_budget_lease_denied", types.TokenUsage{}, nil, started, fp))
		return
	}
	if lease == nil {
		lease = &tokenLease{}
	}
	callCtx, cancel := stageContext(ctx, timeoutSeconds)
	decision, usage, err := safeSafetyCall(callCtx, p.SafetyGuard, task, task.FinalAnswer)
	cancel()
	lease.commit(usage.TotalTokens)
	p.observe(usage, "safety_guard_output")
	if err != nil || decision == nil || strings.TrimSpace(decision.SafeText) == "" {
		report.Stages = append(report.Stages, v2Stage(safetyStage, failureStatus(err), failureReason(err), usage, nil, started, fp))
		return
	}
	task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: safetyStage, Observation: fmt.Sprintf("allowed=%t categories=%s", decision.Allowed, strings.Join(decision.Categories, ",")), TokenUsage: usage})
	task.FinalAnswer = decision.SafeText
	report.Stages = append(report.Stages, v2Stage(safetyStage, "passed", "", usage, nil, started, fp))
	_ = now
}

func auditSnapshot(task *types.Task) *types.Task {
	return auditSnapshotKeeping(task)
}

func auditSnapshotKeeping(task *types.Task, keep ...string) *types.Task {
	snapshot := types.CloneTask(task)
	allowed := make(map[string]bool, len(keep))
	for _, action := range keep {
		allowed[action] = true
	}
	traces := snapshot.Trace[:0]
	for _, trace := range snapshot.Trace {
		if isPipelineAuditAction(trace.Action) && !allowed[trace.Action] {
			continue
		}
		traces = append(traces, trace)
	}
	snapshot.Trace = traces
	snapshot.AnswerAudit = nil
	return snapshot
}

func stageTokenBudget(budgets map[string]int, name string) int {
	if budget := budgets[name]; budget > 0 {
		return budget
	}
	return 0
}

func stageContext(ctx context.Context, timeoutSeconds int) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
}

func stageFingerprint(name, answer, evidenceHash, reference string) string {
	return digest(Version + "\x00" + name + "\x00" + digest(answer) + "\x00" + evidenceHash + "\x00" + reference)
}

func referenceDate(name string, now time.Time) string {
	if name == factfreshness.TraceAction {
		return now.UTC().Format("2006-01-02")
	}
	return ""
}

func reusableStage(report *types.AnswerAuditReport, name, fingerprint string) (types.AnswerAuditStage, bool) {
	if report == nil || report.PipelineVersion != Version {
		return types.AnswerAuditStage{}, false
	}
	for _, item := range report.Stages {
		if item.Name != name || item.Fingerprint != fingerprint {
			continue
		}
		switch item.Status {
		case "passed", "warned", "not_applicable":
			item.DurationMS = 0
			return item, true
		}
	}
	return types.AnswerAuditStage{}, false
}

func dependencyFailed(report *types.AnswerAuditReport, names ...string) bool {
	for _, name := range names {
		for _, item := range report.Stages {
			if item.Name == name && (item.Status == "failed" || item.Status == "budget_insufficient" || item.Status == "dependency_failed") {
				return true
			}
		}
	}
	return false
}

func v2Stage(name, status, reason string, usage types.TokenUsage, findings []types.AnswerAuditFinding, started time.Time, fingerprint string) types.AnswerAuditStage {
	duration := int64(0)
	if !started.IsZero() {
		duration = time.Since(started).Milliseconds()
	}
	return types.AnswerAuditStage{Name: name, Status: status, Reason: reason, TokenUsage: usage, Findings: findings, DurationMS: duration, Fingerprint: fingerprint}
}

func failureStatus(err error) string {
	if err == nil {
		return "failed"
	}
	var budgetErr *llmcore.TaskBudgetError
	if errors.As(err, &budgetErr) {
		return "budget_insufficient"
	}
	return "failed"
}

func failureReason(err error) string {
	if err == nil {
		return "invalid_result"
	}
	var budgetErr *llmcore.TaskBudgetError
	if errors.As(err, &budgetErr) {
		return "runtime_" + boundedAuditValue(budgetErr.Kind, 80)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if strings.HasPrefix(err.Error(), "answer pipeline stage panic:") {
		return "stage_panic"
	}
	return "stage_error"
}

func resultStatusFreshness(result *factfreshness.Result, err error) string {
	if err != nil || result == nil {
		return failureStatus(err)
	}
	if !result.TimeSensitive || result.Status == "not_applicable" {
		return "not_applicable"
	}
	if result.Status == "current" {
		return "passed"
	}
	return "warned"
}

func resultStatusNumeric(result *numericconsistency.Result, err error) string {
	if err != nil || result == nil {
		return failureStatus(err)
	}
	if !result.HasNumericClaims || result.Status == "not_applicable" {
		return "not_applicable"
	}
	if result.Status == "consistent" {
		return "passed"
	}
	return "warned"
}

func freshnessFindings(result *factfreshness.Result) []types.AnswerAuditFinding {
	if result == nil || !result.TimeSensitive || result.Status == "current" || result.Status == "not_applicable" {
		return nil
	}
	return []types.AnswerAuditFinding{{Kind: "staleness", Detail: boundedAuditValue(result.Summary, 500), SourceID: boundedAuditValue(strings.Join(result.SourceIDs, ","), 200)}}
}

func numericFindings(result *numericconsistency.Result) []types.AnswerAuditFinding {
	if result == nil || !result.HasNumericClaims || result.Status == "consistent" || result.Status == "not_applicable" {
		return nil
	}
	return []types.AnswerAuditFinding{{Kind: "numeric_inconsistency", Detail: boundedAuditValue(result.Summary, 500), SourceID: boundedAuditValue(strings.Join(result.SourceIDs, ","), 200)}}
}

func uncertaintyFindings(result *uncertainty.Result) []types.AnswerAuditFinding {
	if result == nil || !result.NeedsQualification {
		return nil
	}
	findings := make([]types.AnswerAuditFinding, 0, len(result.Reasons))
	for _, reason := range result.Reasons {
		findings = append(findings, types.AnswerAuditFinding{Kind: boundedAuditValue(reason, 100), Detail: boundedAuditValue(result.Summary, 500)})
	}
	return findings
}

func finishV2(report *types.AnswerAuditReport, task *types.Task, required []string, onFailure string) {
	report.CompletedAt = time.Now().UTC()
	report.Publishable = strings.TrimSpace(task.FinalAnswer) != ""
	for i := range report.Stages {
		if report.Stages[i].Fingerprint == "" {
			report.Stages[i].Fingerprint = digest(Version + "\x00" + report.Stages[i].Name + "\x00" + report.DraftHash + "\x00" + report.EvidenceHash)
		}
	}
	failedRequired := false
	for _, name := range required {
		found, satisfied := false, false
		for _, item := range report.Stages {
			if item.Name != name {
				continue
			}
			found = true
			satisfied = item.Status == "passed" || item.Status == "warned" || item.Status == "not_applicable"
			break
		}
		if !found || !satisfied {
			failedRequired = true
			break
		}
	}
	if failedRequired && report.Enforcement != "observe" {
		if strings.EqualFold(strings.TrimSpace(onFailure), "failed") {
			task.Status = types.StatusFailed
		} else if task.Status == types.StatusCompleted {
			task.Status = types.StatusPartial
		}
		if report.Enforcement == "strict" {
			report.Publishable = false
		}
	}
	task.AnswerAudit = report
}

func safeCitationCall(ctx context.Context, checker planner.CitationVerifier, task *types.Task, answer string) (result *planner.CitationVerification, usage types.TokenUsage, err error) {
	defer recoverStagePanic(&err)
	return checker.Verify(ctx, types.CloneTask(task), answer)
}

func safeFreshnessCall(ctx context.Context, checker factfreshness.Checker, task *types.Task, answer string) (result *factfreshness.Result, usage types.TokenUsage, err error) {
	defer recoverStagePanic(&err)
	return checker.Check(ctx, types.CloneTask(task), answer)
}

func safeNumericCall(ctx context.Context, checker numericconsistency.Checker, task *types.Task, answer string) (result *numericconsistency.Result, usage types.TokenUsage, err error) {
	defer recoverStagePanic(&err)
	return checker.Check(ctx, types.CloneTask(task), answer)
}

func safeUncertaintyCall(ctx context.Context, calibrator uncertainty.Calibrator, task *types.Task, answer string) (result *uncertainty.Result, usage types.TokenUsage, err error) {
	defer recoverStagePanic(&err)
	return calibrator.Calibrate(ctx, types.CloneTask(task), answer)
}

func safeSafetyCall(ctx context.Context, guard policy.SafetyGuard, task *types.Task, answer string) (decision *policy.SafetyDecision, usage types.TokenUsage, err error) {
	defer recoverStagePanic(&err)
	return guard.Evaluate(ctx, policy.SafetyStageOutput, types.CloneTask(task), answer)
}

func recoverStagePanic(err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("answer pipeline stage panic: %v", recovered)
	}
}
