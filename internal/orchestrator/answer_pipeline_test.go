package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/answerpipeline"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

type pipelineCapture struct {
	status types.TaskStatus
	answer string
	calls  int
}

func (p *pipelineCapture) Process(_ context.Context, task *types.Task, _ string) (*types.AnswerAuditReport, error) {
	p.calls++
	p.status = task.Status
	p.answer = task.FinalAnswer
	report := &types.AnswerAuditReport{Publishable: true}
	task.AnswerAudit = report
	return report, nil
}

type pipelineErrorPlanner struct{}

func (pipelineErrorPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	return nil, errors.New("boom")
}

func TestPipelineRunsAfterFailureNormalization(t *testing.T) {
	pipeline := &pipelineCapture{}
	engine := &Engine{Planner: pipelineErrorPlanner{}, Mode: ModeLegacy, AnswerPipeline: pipeline}
	task := &types.Task{ID: "failed", Status: types.StatusRunning, MaxSteps: 2, ToolBudget: 1}
	if err := engine.Next(context.Background(), task); err == nil {
		t.Fatal("expected execution error")
	}
	if pipeline.calls != 1 || pipeline.status != types.StatusFailed || pipeline.answer != "Failed: boom" {
		t.Fatalf("pipeline=%+v task=%+v", pipeline, task)
	}
}

type multiAgentDraftPlanner struct{}

func (multiAgentDraftPlanner) Plan(context.Context, string, string, []types.Memory) (*multiagent.ResearchPlan, error) {
	return &multiagent.ResearchPlan{ThoughtSummary: "no tools required"}, nil
}

func (multiAgentDraftPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*multiagent.ResearchPlan, error) {
	return &multiagent.ResearchPlan{}, nil
}

type multiAgentDraftWriter struct{}

func (multiAgentDraftWriter) Write(context.Context, string, []multiagent.StepEvidence, []types.Memory) (*multiagent.WriterOutput, error) {
	return &multiagent.WriterOutput{FinalAnswer: "candidate answer", EvidenceSummary: "partial evidence", DraftConfidence: "high"}, nil
}

type multiAgentDraftVerifier struct{}

func (multiAgentDraftVerifier) Verify(context.Context, string, string, []multiagent.StepEvidence) (*multiagent.VerificationResult, error) {
	return &multiagent.VerificationResult{Supported: false, Issues: []multiagent.VerificationIssue{{Kind: "unsupported_claim", Detail: "claim lacks a cited source", SourceID: "draft-claim-1"}}}, nil
}

type multiAgentUncertainty struct{}

func (multiAgentUncertainty) Calibrate(context.Context, *types.Task, string) (*uncertainty.Result, types.TokenUsage, error) {
	return &uncertainty.Result{Confidence: "low", NeedsQualification: true, Reasons: []string{"unsupported_claim"}, Summary: "draft verifier finding"}, types.TokenUsage{}, nil
}

func TestMultiAgentDraftFlowsThroughUnifiedPipeline(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.RAG.ContextMode = "prefetch"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneAnswerVerifier:              {},
			config.LLMSceneAnswerUncertaintyCalibrator: {},
		}
	}))
	coordinator := &multiagent.Coordinator{
		Planner:  multiAgentDraftPlanner{},
		Writer:   multiAgentDraftWriter{},
		Verifier: multiAgentDraftVerifier{},
	}
	pipeline := &answerpipeline.DefaultPipeline{UncertaintyCalibrator: multiAgentUncertainty{}}
	engine := &Engine{Mode: ModeMultiAgent, Coordinator: coordinator, AnswerPipeline: pipeline}
	task := &types.Task{ID: "multiagent-p1", Goal: "answer with evidence", Status: types.StatusCreated, MaxSteps: 5}

	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || task.AnswerAudit == nil || task.AnswerAudit.FinalConfidence != "low" {
		t.Fatalf("task = %+v", task)
	}
	if len(task.Unresolved) != 1 || task.Unresolved[0] != "final_answer_not_fully_supported" {
		t.Fatalf("unresolved = %v", task.Unresolved)
	}
	if len(task.AnswerAudit.Stages) != 6 || task.AnswerAudit.Stages[0].Name != "answer_verify" {
		t.Fatalf("audit stages = %+v", task.AnswerAudit.Stages)
	}
	finding := task.AnswerAudit.Stages[0].Findings[0]
	if finding.Kind != "unsupported_claim" || finding.SourceID != "draft-claim-1" {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestReauditBypassesTerminalShortCircuitAndSupportsForce(t *testing.T) {
	pipeline := &pipelineCapture{}
	engine := &Engine{Mode: ModeLegacy, AnswerPipeline: pipeline}
	task := &types.Task{
		ID: "reaudit", Status: types.StatusCompleted, FinalAnswer: "answer",
		AnswerAudit: &types.AnswerAuditReport{PipelineVersion: "old"},
	}
	report, err := engine.Reaudit(context.Background(), task, true)
	if err != nil {
		t.Fatal(err)
	}
	if pipeline.calls != 1 || report == nil || task.AnswerAudit != report {
		t.Fatalf("pipeline=%+v task=%+v report=%+v", pipeline, task, report)
	}
	nonTerminal := &types.Task{Status: types.StatusRunning, FinalAnswer: "draft"}
	if _, err := engine.Reaudit(context.Background(), nonTerminal, false); err == nil {
		t.Fatal("expected non-terminal re-audit rejection")
	}
}
