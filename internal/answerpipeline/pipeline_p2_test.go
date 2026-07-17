package answerpipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type auditConcurrencyProbe struct {
	mu      sync.Mutex
	current int
	max     int
}

func (p *auditConcurrencyProbe) enter() func() {
	p.mu.Lock()
	p.current++
	if p.current > p.max {
		p.max = p.current
	}
	p.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	return func() {
		p.mu.Lock()
		p.current--
		p.mu.Unlock()
	}
}

type parallelFreshness struct {
	probe *auditConcurrencyProbe
	calls int
	usage int
}

func (c *parallelFreshness) Check(context.Context, *types.Task, string) (*factfreshness.Result, types.TokenUsage, error) {
	c.calls++
	done := c.probe.enter()
	defer done()
	return &factfreshness.Result{Status: "not_applicable", Summary: "not temporal"}, types.TokenUsage{TotalTokens: c.usage}, nil
}

type parallelNumeric struct {
	probe *auditConcurrencyProbe
	calls int
	usage int
	panic bool
}

func (c *parallelNumeric) Check(context.Context, *types.Task, string) (*numericconsistency.Result, types.TokenUsage, error) {
	c.calls++
	if c.panic {
		panic("numeric checker panic")
	}
	done := c.probe.enter()
	defer done()
	return &numericconsistency.Result{Status: "not_applicable", Summary: "no numeric claim"}, types.TokenUsage{TotalTokens: c.usage}, nil
}

type fixedOutputGuard struct {
	calls int
	usage int
}

func (g *fixedOutputGuard) Evaluate(context.Context, policy.SafetyStage, *types.Task, string) (*policy.SafetyDecision, types.TokenUsage, error) {
	g.calls++
	return &policy.SafetyDecision{Allowed: true, SafeText: "safe answer"}, types.TokenUsage{TotalTokens: g.usage}, nil
}

func auditTask(answer string) *types.Task {
	return &types.Task{FinalAnswer: answer, Status: types.StatusCompleted, Trace: []types.StepTrace{{
		Action: "web_search", Observation: "source",
		Evidence: []types.Evidence{{Path: "source-1", Query: "fact", Lines: []string{"version 2 costs 10"}}},
	}}}
}

func TestParallelEvidenceAuditsUseSnapshotsAndApplyDeterministically(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.ParallelAudits = true
		cfg.AnswerPipeline.AuditTokenReserve = 2000
		cfg.AnswerPipeline.StageTokenBudgets = map[string]int{factfreshness.TraceAction: 500, numericconsistency.TraceAction: 500}
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}, config.LLMSceneNumericConsistencyChecker: {}}
	}))
	probe := &auditConcurrencyProbe{}
	fresh := &parallelFreshness{probe: probe}
	numeric := &parallelNumeric{probe: probe}
	task := auditTask("version 2 costs 10")
	report, err := (&DefaultPipeline{FreshnessChecker: fresh, NumericChecker: numeric}).Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	if probe.max != 2 {
		t.Fatalf("max concurrency = %d, want 2", probe.max)
	}
	if report.Stages[1].Name != factfreshness.TraceAction || report.Stages[2].Name != numericconsistency.TraceAction {
		t.Fatalf("stage order = %+v", report.Stages)
	}
	auditActions := make([]string, 0, 2)
	for _, trace := range task.Trace {
		if trace.Action == factfreshness.TraceAction || trace.Action == numericconsistency.TraceAction {
			auditActions = append(auditActions, trace.Action)
		}
	}
	if len(auditActions) != 2 || auditActions[0] != factfreshness.TraceAction || auditActions[1] != numericconsistency.TraceAction {
		t.Fatalf("trace apply order = %v", auditActions)
	}
}

func TestCostBudgetForcesEvidenceAuditsToRunSerially(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.ParallelAudits = true
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}, config.LLMSceneNumericConsistencyChecker: {}}
	}))
	probe := &auditConcurrencyProbe{}
	task := auditTask("version 2 costs 10")
	task.LLMCostBudgetUSD = 1
	ctx := llmcore.WithTaskBudget(context.Background(), task)
	_, err := (&DefaultPipeline{FreshnessChecker: &parallelFreshness{probe: probe}, NumericChecker: &parallelNumeric{probe: probe}}).Process(ctx, task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if probe.max != 1 {
		t.Fatalf("max concurrency with cost budget = %d, want 1", probe.max)
	}
}

func TestAuditLeaseDenialProducesDependencyFailure(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.ParallelAudits = true
		cfg.AnswerPipeline.AuditTokenReserve = 500
		cfg.AnswerPipeline.StageTokenBudgets = map[string]int{factfreshness.TraceAction: 300, numericconsistency.TraceAction: 300}
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}, config.LLMSceneNumericConsistencyChecker: {}, config.LLMSceneAnswerUncertaintyCalibrator: {}}
	}))
	probe := &auditConcurrencyProbe{}
	task := auditTask("version 2 costs 10")
	task.TokenBudget = 1000
	task.Trace[0].TokenUsage.TotalTokens = 500
	report, err := (&DefaultPipeline{FreshnessChecker: &parallelFreshness{probe: probe, usage: 100}, NumericChecker: &parallelNumeric{probe: probe}}).Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if stageStatus(report, numericconsistency.TraceAction) != "budget_insufficient" || stageStatus(report, "answer_uncertainty_calibrate") != "dependency_failed" {
		t.Fatalf("stages = %+v", report.Stages)
	}
}

func TestSafetyLeaseHasPriorityOverEvidenceAudit(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.AuditTokenReserve = 600
		cfg.AnswerPipeline.StageTokenBudgets = map[string]int{factfreshness.TraceAction: 600, safetyStage: 600}
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}, config.LLMSceneSafetyGuard: {}}
	}))
	guard := &fixedOutputGuard{usage: 600}
	task := auditTask("version 2")
	task.TokenBudget = 600
	report, err := (&DefaultPipeline{FreshnessChecker: &parallelFreshness{probe: &auditConcurrencyProbe{}}, SafetyGuard: guard}).Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if stageStatus(report, factfreshness.TraceAction) != "budget_insufficient" || stageStatus(report, safetyStage) != "passed" || guard.calls != 1 {
		t.Fatalf("stages=%+v guard_calls=%d", report.Stages, guard.calls)
	}
}

func TestStageFingerprintCachesAndFreshnessDateInvalidates(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}}
	}))
	checker := &parallelFreshness{probe: &auditConcurrencyProbe{}}
	pipeline := &DefaultPipeline{FreshnessChecker: checker, Now: func() time.Time { return now }}
	task := auditTask("current version")
	first, err := pipeline.Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint := first.Stages[1].Fingerprint
	task = types.CloneTask(task)
	if _, err := pipeline.Process(context.Background(), task, "legacy"); err != nil {
		t.Fatal(err)
	}
	if checker.calls != 1 || firstFingerprint == "" {
		t.Fatalf("calls=%d fingerprint=%q", checker.calls, firstFingerprint)
	}
	now = now.Add(24 * time.Hour)
	third, err := pipeline.Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if checker.calls != 2 || third.Stages[1].Fingerprint == firstFingerprint {
		t.Fatalf("calls=%d first=%q third=%q", checker.calls, firstFingerprint, third.Stages[1].Fingerprint)
	}
}

func TestEvidenceDigestChangeInvalidatesStageCache(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneFactFreshnessChecker: {}}
	}))
	checker := &parallelFreshness{probe: &auditConcurrencyProbe{}}
	pipeline := &DefaultPipeline{FreshnessChecker: checker}
	task := auditTask("current version")
	if _, err := pipeline.Process(context.Background(), task, "legacy"); err != nil {
		t.Fatal(err)
	}
	task.Trace[0].Observation = "updated source observation"
	if _, err := pipeline.Process(context.Background(), task, "legacy"); err != nil {
		t.Fatal(err)
	}
	if checker.calls != 2 {
		t.Fatalf("calls after evidence change = %d, want 2", checker.calls)
	}
}

func TestStrictRequiredStageFailureBlocksPublishing(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "strict"
		cfg.AnswerPipeline.RequiredStages = []string{safetyStage}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	task := auditTask("answer")
	report, err := (&DefaultPipeline{}).Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if report.Publishable || task.Status != types.StatusPartial || stageStatus(report, safetyStage) != "disabled" {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
}

func TestTenantPipelinePolicyOverridesGlobalEnforcement(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "observe"
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"strict-tenant": {
				AnswerPipelineEnforcement:    "strict",
				AnswerPipelineRequiredStages: []string{safetyStage},
			},
		}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	task := auditTask("answer")
	task.TenantID = "strict-tenant"
	report, err := (&DefaultPipeline{}).Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if report.Enforcement != "strict" || report.Publishable || task.Status != types.StatusPartial {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
}

func TestStagePanicIsIsolatedAndAdvisoryFailureIsPartial(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "advisory"
		cfg.AnswerPipeline.RequiredStages = []string{numericconsistency.TraceAction}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneNumericConsistencyChecker: {}}
	}))
	task := auditTask("value 10")
	report, err := (&DefaultPipeline{NumericChecker: &parallelNumeric{probe: &auditConcurrencyProbe{}, panic: true}}).Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if stageStatus(report, numericconsistency.TraceAction) != "failed" || task.Status != types.StatusPartial || !report.Publishable {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
}

func stageStatus(report *types.AnswerAuditReport, name string) string {
	for _, stage := range report.Stages {
		if stage.Name == name {
			return stage.Status
		}
	}
	return ""
}
