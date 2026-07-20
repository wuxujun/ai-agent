package multiagent

import (
	"context"
	"fmt"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedWriter struct{}

func (fixedWriter) Write(context.Context, string, []StepEvidence, []types.Memory) (*WriterOutput, error) {
	return &WriterOutput{FinalAnswer: "answer", EvidenceSummary: "summary", Confidence: "high", TokenUsage: types.TokenUsage{TotalTokens: 10}}, nil
}

type fixedVerifier struct{}

func (fixedVerifier) Verify(context.Context, string, string, []StepEvidence) (*VerificationResult, error) {
	return &VerificationResult{Supported: false, Issues: []VerificationIssue{{Kind: "evidence_gap", Detail: "missing source", SourceID: "step-1"}}, TokenUsage: types.TokenUsage{TotalTokens: 4}}, nil
}

func TestRunWritePhaseVerifierLowersConfidenceAndTracksUsage(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneAnswerVerifier: {}}
	}))
	task := &types.Task{ID: "verify", Goal: "goal"}
	c := &Coordinator{Writer: fixedWriter{}, Verifier: fixedVerifier{}}
	confidence, err := c.runWritePhase(context.Background(), task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if confidence != "low" {
		t.Fatalf("confidence = %q", confidence)
	}
	if len(task.Trace) != 1 || task.Trace[0].TokenUsage.TotalTokens != 14 {
		t.Fatalf("trace usage = %+v", task.Trace)
	}
	if len(task.Trace[0].Evidence) != 1 || task.Trace[0].Evidence[0].Path != types.AnswerVerifierEvidencePrefix+"step-1" || task.Trace[0].Evidence[0].Query != "evidence_gap" {
		t.Fatalf("structured verifier evidence = %+v", task.Trace[0].Evidence)
	}
	if task.AnswerAudit != nil {
		t.Fatal("coordinator must produce a draft, not final answer audit")
	}
}

func TestValidateVerificationResultRejectsInconsistentPayload(t *testing.T) {
	cases := []*VerificationResult{
		{Supported: true, Issues: []VerificationIssue{{Kind: "evidence_gap", Detail: "gap"}}},
		{Supported: false},
		{Supported: false, Issues: []VerificationIssue{{Kind: "unknown", Detail: "gap"}}},
		{Supported: false, Issues: []VerificationIssue{{Kind: "evidence_gap"}}},
	}
	for _, result := range cases {
		if err := validateVerificationResult(result); err == nil {
			t.Fatalf("expected invalid result: %+v", result)
		}
	}
}

type sequentialFinalVerifierCaller struct {
	calls int
}

func (c *sequentialFinalVerifierCaller) CallJSON(_ context.Context, _ llmcore.Config, _, _ string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.calls++
	switch output := dest.(type) {
	case *finalAnswerCandidate:
		output.FinalAnswer = "candidate answer"
		output.EvidenceSummary = "step-1 evidence"
		output.DraftConfidence = "high"
		return types.TokenUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, nil
	case *VerificationResult:
		output.Supported = false
		output.Issues = []VerificationIssue{{Kind: "evidence_gap", Detail: "missing independent source", SourceID: "step-1"}}
		return types.TokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}, nil
	default:
		return types.TokenUsage{}, fmt.Errorf("unexpected verifier output type %T", dest)
	}
}

func TestFinalVerifierUsesIndependentDraftAndVerificationCalls(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Langfuse.Enabled = false
	}))
	caller := &sequentialFinalVerifierCaller{}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))

	output, err := (&VerifierAgent{}).Finalize(ctx, "goal", []StepEvidence{{StepID: "step-1", Observation: "evidence"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if caller.calls != 2 {
		t.Fatalf("LLM calls = %d, want 2", caller.calls)
	}
	if output.Supported || output.DraftConfidence != "low" {
		t.Fatalf("output = %+v", output)
	}
	if output.TokenUsage != (types.TokenUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11}) {
		t.Fatalf("token usage = %+v", output.TokenUsage)
	}
}
