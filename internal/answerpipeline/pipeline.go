package answerpipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

const Version = "p1-v1"

type TokenObserver func(types.TokenUsage, string)

type Pipeline interface {
	Process(context.Context, *types.Task, string) (*types.AnswerAuditReport, error)
}

type DefaultPipeline struct {
	CitationVerifier      planner.CitationVerifier
	FreshnessChecker      factfreshness.Checker
	NumericChecker        numericconsistency.Checker
	UncertaintyCalibrator uncertainty.Calibrator
	SafetyGuard           policy.SafetyGuard
	SceneEnabled          func(string) bool
	ObserveTokens         TokenObserver
}

func (p *DefaultPipeline) enabled(scene string) bool {
	if p.SceneEnabled != nil {
		return p.SceneEnabled(scene)
	}
	_, ok := config.Get().LLM.Scenes[scene]
	return ok
}

func (p *DefaultPipeline) Process(ctx context.Context, task *types.Task, mode string) (*types.AnswerAuditReport, error) {
	if task == nil {
		return nil, fmt.Errorf("answer pipeline requires task")
	}
	cfg := config.Get().AnswerPipeline
	if !cfg.Enabled {
		return nil, nil
	}
	started := time.Now().UTC()
	report := &types.AnswerAuditReport{PipelineVersion: Version, DraftHash: digest(task.FinalAnswer), EvidenceHash: evidenceDigest(task), StartedAt: started, Enforcement: normalizedEnforcement(cfg.Enforcement), Publishable: strings.TrimSpace(task.FinalAnswer) != ""}

	inputRefusal := hasBlockedInput(task)
	if inputRefusal {
		report.Stages = append(report.Stages,
			stage("citation_verify", "not_applicable", "covered_by_input_guard", types.TokenUsage{}, nil, 0),
			stage(factfreshness.TraceAction, "not_applicable", "covered_by_input_guard", types.TokenUsage{}, nil, 0),
			stage(numericconsistency.TraceAction, "not_applicable", "covered_by_input_guard", types.TokenUsage{}, nil, 0),
			stage(uncertainty.TraceAction, "not_applicable", "covered_by_input_guard", types.TokenUsage{}, nil, 0),
			stage("safety_guard_output", "passed", "covered_by_input_guard", types.TokenUsage{}, nil, 0),
		)
		finalizeReport(report, task)
		task.AnswerAudit = report
		return report, nil
	}

	p.importVerifierFindings(task, report)
	p.runCitation(ctx, task, report)
	p.runFreshness(ctx, task, report)
	p.runNumeric(ctx, task, report)
	p.runUncertainty(ctx, task, report)
	p.runSafety(ctx, task, report)
	finalizeReport(report, task)
	task.AnswerAudit = report
	return report, nil
}

// importVerifierFindings promotes draft-phase verifier output into the common
// audit report. The original trace evidence remains available to uncertainty,
// while the report gives API consumers a stable, typed representation.
func (p *DefaultPipeline) importVerifierFindings(task *types.Task, report *types.AnswerAuditReport) {
	findings := verifierFindings(task)
	if len(findings) == 0 {
		return
	}
	report.Stages = append(report.Stages, stage("answer_verify", "warned", "draft verifier findings", types.TokenUsage{}, findings, time.Now()))
}

func (p *DefaultPipeline) runCitation(ctx context.Context, task *types.Task, report *types.AnswerAuditReport) {
	started := time.Now()
	if p.CitationVerifier == nil || !p.enabled(config.LLMSceneCitationVerifier) {
		report.Stages = append(report.Stages, stage("citation_verify", "disabled", "verifier unavailable or scene disabled", types.TokenUsage{}, nil, started))
		return
	}
	if !planner.HasCitationEvidence(task) {
		report.Stages = append(report.Stages, stage("citation_verify", "not_applicable", "no citation evidence", types.TokenUsage{}, nil, started))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneCitationVerifier, task) {
		report.Stages = append(report.Stages, stage("citation_verify", "budget_insufficient", "token gate", types.TokenUsage{}, nil, started))
		return
	}
	result, usage, err := p.CitationVerifier.Verify(ctx, types.CloneTask(task), task.FinalAnswer)
	p.observe(usage, "citation_verifier")
	if err != nil || result == nil || strings.TrimSpace(result.VerifiedAnswer) == "" {
		report.Stages = append(report.Stages, stage("citation_verify", "failed", errorReason(err), usage, nil, started))
		return
	}
	task.FinalAnswer = result.VerifiedAnswer
	task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: "citation_verify", Observation: fmt.Sprintf("supported=%t unsupported_claims=%d citation_issues=%d", result.Supported, len(result.UnsupportedClaims), len(result.CitationIssues)), TokenUsage: usage})
	status := "passed"
	if !result.Supported {
		status = "warned"
	}
	report.Stages = append(report.Stages, stage("citation_verify", status, "", usage, nil, started))
}

func (p *DefaultPipeline) runFreshness(ctx context.Context, task *types.Task, report *types.AnswerAuditReport) {
	started := time.Now()
	if p.FreshnessChecker == nil || !p.enabled(config.LLMSceneFactFreshnessChecker) {
		report.Stages = append(report.Stages, stage(factfreshness.TraceAction, "disabled", "checker unavailable or scene disabled", types.TokenUsage{}, nil, started))
		return
	}
	if !factfreshness.ShouldCheck(task) {
		report.Stages = append(report.Stages, stage(factfreshness.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneFactFreshnessChecker, task) {
		report.Stages = append(report.Stages, stage(factfreshness.TraceAction, "budget_insufficient", "token gate", types.TokenUsage{}, nil, started))
		return
	}
	result, usage, err := p.FreshnessChecker.Check(ctx, types.CloneTask(task), task.FinalAnswer)
	factfreshness.Apply(task, result, usage, err)
	p.observe(usage, "fact_freshness_checker")
	status := "passed"
	if err != nil {
		status = "failed"
	} else if result != nil && result.TimeSensitive && result.Status != "current" {
		status = "warned"
	}
	report.Stages = append(report.Stages, stage(factfreshness.TraceAction, status, errorReason(err), usage, nil, started))
}

func (p *DefaultPipeline) runNumeric(ctx context.Context, task *types.Task, report *types.AnswerAuditReport) {
	started := time.Now()
	if p.NumericChecker == nil || !p.enabled(config.LLMSceneNumericConsistencyChecker) {
		report.Stages = append(report.Stages, stage(numericconsistency.TraceAction, "disabled", "checker unavailable or scene disabled", types.TokenUsage{}, nil, started))
		return
	}
	if !numericconsistency.ShouldCheck(task) {
		report.Stages = append(report.Stages, stage(numericconsistency.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneNumericConsistencyChecker, task) {
		report.Stages = append(report.Stages, stage(numericconsistency.TraceAction, "budget_insufficient", "token gate", types.TokenUsage{}, nil, started))
		return
	}
	result, usage, err := p.NumericChecker.Check(ctx, types.CloneTask(task), task.FinalAnswer)
	numericconsistency.Apply(task, result, usage, err)
	p.observe(usage, "numeric_consistency_checker")
	status := "passed"
	if err != nil {
		status = "failed"
	} else if result != nil && result.HasNumericClaims && result.Status != "consistent" {
		status = "warned"
	}
	report.Stages = append(report.Stages, stage(numericconsistency.TraceAction, status, errorReason(err), usage, nil, started))
}

func (p *DefaultPipeline) runUncertainty(ctx context.Context, task *types.Task, report *types.AnswerAuditReport) {
	started := time.Now()
	if p.UncertaintyCalibrator == nil || !p.enabled(config.LLMSceneAnswerUncertaintyCalibrator) {
		report.Stages = append(report.Stages, stage(uncertainty.TraceAction, "disabled", "calibrator unavailable or scene disabled", types.TokenUsage{}, nil, started))
		return
	}
	if !uncertainty.ShouldCalibrate(task) {
		report.Stages = append(report.Stages, stage(uncertainty.TraceAction, "not_applicable", "eligibility", types.TokenUsage{}, nil, started))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneAnswerUncertaintyCalibrator, task) {
		report.Stages = append(report.Stages, stage(uncertainty.TraceAction, "budget_insufficient", "token gate", types.TokenUsage{}, nil, started))
		return
	}
	result, usage, err := p.UncertaintyCalibrator.Calibrate(ctx, types.CloneTask(task), task.FinalAnswer)
	uncertainty.Apply(task, result, usage, err)
	p.observe(usage, "answer_uncertainty_calibrator")
	status := "passed"
	if err != nil {
		status = "failed"
	} else if result != nil {
		report.FinalConfidence = result.Confidence
		if result.NeedsQualification {
			status = "warned"
		}
	}
	report.Stages = append(report.Stages, stage(uncertainty.TraceAction, status, errorReason(err), usage, nil, started))
}

func (p *DefaultPipeline) runSafety(ctx context.Context, task *types.Task, report *types.AnswerAuditReport) {
	started := time.Now()
	if p.SafetyGuard == nil || !p.enabled(config.LLMSceneSafetyGuard) {
		report.Stages = append(report.Stages, stage("safety_guard_output", "disabled", "guard unavailable or scene disabled", types.TokenUsage{}, nil, started))
		return
	}
	if !llmcore.AllowedForTask(config.LLMSceneSafetyGuard, task) {
		report.Stages = append(report.Stages, stage("safety_guard_output", "budget_insufficient", "token gate", types.TokenUsage{}, nil, started))
		return
	}
	decision, usage, err := p.SafetyGuard.Evaluate(ctx, policy.SafetyStageOutput, types.CloneTask(task), task.FinalAnswer)
	p.observe(usage, "safety_guard_output")
	if err != nil || decision == nil || strings.TrimSpace(decision.SafeText) == "" {
		report.Stages = append(report.Stages, stage("safety_guard_output", "failed", errorReason(err), usage, nil, started))
		return
	}
	task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: "safety_guard_output", Observation: fmt.Sprintf("allowed=%t categories=%s", decision.Allowed, strings.Join(decision.Categories, ",")), TokenUsage: usage})
	task.FinalAnswer = decision.SafeText
	report.Stages = append(report.Stages, stage("safety_guard_output", "passed", "", usage, nil, started))
}

func (p *DefaultPipeline) observe(usage types.TokenUsage, operation string) {
	if p.ObserveTokens != nil {
		p.ObserveTokens(usage, operation)
	}
}

func stage(name, status, reason string, usage types.TokenUsage, findings []types.AnswerAuditFinding, started any) types.AnswerAuditStage {
	duration := int64(0)
	if t, ok := started.(time.Time); ok {
		duration = time.Since(t).Milliseconds()
	}
	return types.AnswerAuditStage{Name: name, Status: status, Reason: reason, TokenUsage: usage, Findings: findings, DurationMS: duration}
}
func errorReason(err error) string {
	if err == nil {
		return ""
	}
	value := sanitize.Secrets(err.Error())
	if len([]rune(value)) > 500 {
		value = string([]rune(value)[:500])
	}
	return value
}
func normalizedEnforcement(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "observe"
	}
	return v
}
func digest(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func evidenceDigest(task *types.Task) string {
	var items []string
	for _, tr := range task.Trace {
		for _, ev := range tr.Evidence {
			items = append(items, tr.Action+"\x00"+ev.Path+"\x00"+ev.Query+"\x00"+digest(strings.Join(ev.Lines, "\n")))
		}
	}
	for _, mem := range task.Memories {
		items = append(items, "memory\x00"+mem.ID+"\x00"+digest(mem.Goal+"\n"+mem.KeyFindings+"\n"+mem.FinalAnswer))
	}
	sort.Strings(items)
	return digest(strings.Join(items, "\n"))
}
func hasBlockedInput(task *types.Task) bool {
	for _, tr := range task.Trace {
		if tr.Action == "safety_guard_input" && strings.Contains(tr.Observation, "allowed=false") {
			return true
		}
	}
	return false
}

func verifierFindings(task *types.Task) []types.AnswerAuditFinding {
	if task == nil {
		return nil
	}
	var findings []types.AnswerAuditFinding
	for _, trace := range task.Trace {
		for _, evidence := range trace.Evidence {
			if !strings.HasPrefix(evidence.Path, types.AnswerVerifierEvidencePrefix) {
				continue
			}
			detail := strings.TrimSpace(strings.Join(evidence.Lines, "\n"))
			if detail == "" {
				continue
			}
			switch evidence.Query {
			case "unsupported_claim", "evidence_gap", "contradiction":
			default:
				continue
			}
			sourceID := sanitize.Secrets(strings.TrimPrefix(evidence.Path, types.AnswerVerifierEvidencePrefix))
			if runes := []rune(sourceID); len(runes) > 200 {
				sourceID = string(runes[:200])
			}
			findings = append(findings, types.AnswerAuditFinding{
				Kind:     evidence.Query,
				Detail:   errorReason(fmt.Errorf("%s", detail)),
				SourceID: sourceID,
			})
		}
	}
	return findings
}

func finalizeReport(report *types.AnswerAuditReport, task *types.Task) {
	report.CompletedAt = time.Now().UTC()
	report.Publishable = strings.TrimSpace(task.FinalAnswer) != ""
	for i := range report.Stages {
		if report.Stages[i].Fingerprint == "" {
			report.Stages[i].Fingerprint = digest(Version + "\x00" + report.Stages[i].Name + "\x00" + task.FinalAnswer + "\x00" + report.EvidenceHash)
		}
	}
}
