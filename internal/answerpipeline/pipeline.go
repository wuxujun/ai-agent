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
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

const Version = "p2-v1"

type TokenObserver func(types.TokenUsage, string)
type ReportObserver func(string, *types.AnswerAuditReport)

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
	ObserveReport         ReportObserver
	Now                   func() time.Time
}

func (p *DefaultPipeline) enabled(scene string) bool {
	if p.SceneEnabled != nil {
		return p.SceneEnabled(scene)
	}
	_, ok := config.Get().LLM.Scenes[scene]
	return ok
}

func (p *DefaultPipeline) Process(ctx context.Context, task *types.Task, mode string) (*types.AnswerAuditReport, error) {
	return p.processV2(ctx, task, mode)
}

// importVerifierFindings promotes draft-phase verifier output into the common
// audit report. The original trace evidence remains available to uncertainty,
// while the report gives API consumers a stable, typed representation.
func (p *DefaultPipeline) importVerifierFindings(task *types.Task, report *types.AnswerAuditReport) {
	findings := verifierFindings(task)
	if len(findings) == 0 {
		return
	}
	fingerprint := stageFingerprint("answer_verify", task.FinalAnswer, report.EvidenceHash, "")
	report.Stages = append(report.Stages, v2Stage("answer_verify", "warned", "draft verifier findings", types.TokenUsage{}, findings, time.Now(), fingerprint))
}

func (p *DefaultPipeline) observe(usage types.TokenUsage, operation string) {
	if p.ObserveTokens != nil {
		p.ObserveTokens(usage, operation)
	}
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
		if isPipelineAuditAction(tr.Action) {
			continue
		}
		if observation := boundedAuditValue(tr.Observation, 1200); observation != "" {
			items = append(items, boundedAuditValue(tr.Action, 200)+"\x00observation\x00"+boundedAuditValue(tr.Query, 200)+"\x00"+digest(observation))
		}
		for _, ev := range tr.Evidence {
			content := boundedAuditValue(strings.Join(ev.Lines, "\n"), 1200)
			items = append(items, boundedAuditValue(tr.Action, 200)+"\x00"+boundedAuditValue(ev.Path, 500)+"\x00"+boundedAuditValue(ev.Query, 200)+"\x00"+digest(content))
		}
	}
	for _, mem := range task.Memories {
		items = append(items, "memory\x00"+boundedAuditValue(mem.ID, 200)+"\x00"+digest(boundedAuditValue(mem.Goal+"\n"+mem.KeyFindings+"\n"+mem.FinalAnswer, 1200)))
	}
	sort.Strings(items)
	return digest(strings.Join(items, "\n"))
}

func boundedAuditValue(value string, limit int) string {
	value = sanitize.Secrets(strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func isPipelineAuditAction(action string) bool {
	switch action {
	case "citation_verify", factfreshness.TraceAction, numericconsistency.TraceAction, uncertainty.TraceAction, "safety_guard_output":
		return true
	default:
		return false
	}
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
