package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
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
