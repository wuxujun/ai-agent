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
	return &VerificationResult{Supported: false, Issues: []string{"missing source"}, TokenUsage: types.TokenUsage{TotalTokens: 4}}, nil
}

func TestRunWritePhaseVerifierLowersConfidenceAndTracksUsage(t *testing.T) {
	original := config.Get().LLM.Scenes
	config.Get().LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneAnswerVerifier: {}}
	t.Cleanup(func() { config.Get().LLM.Scenes = original })
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
}
