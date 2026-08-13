package multiagent

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestValidateGeneratedAnswer(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		wantErr bool
	}{
		{name: "empty", answer: "  ", wantErr: true},
		{name: "ellipsis", answer: "...", wantErr: true},
		{name: "unicode ellipsis", answer: "…", wantErr: true},
		{name: "known placeholder", answer: " N/A ", wantErr: true},
		{name: "punctuation only", answer: "?! --", wantErr: true},
		{name: "too short", answer: "a", wantErr: true},
		{name: "short numeric answer", answer: "42"},
		{name: "substantive answer", answer: "The evidence supports this conclusion."},
		{name: "chinese answer", answer: "证据不足，暂时无法确认。"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGeneratedAnswer(tt.answer)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateGeneratedAnswer(%q) error = %v, wantErr=%t", tt.answer, err, tt.wantErr)
			}
		})
	}
}

type answerSequenceWriter struct {
	answers []string
	calls   int
}

func (w *answerSequenceWriter) Write(ctx context.Context, _ string, _ []StepEvidence, _ []types.Memory) (*WriterOutput, error) {
	answer := w.answers[w.calls]
	w.calls++
	if callback := answerTokenCallbackFromContext(ctx); callback != nil {
		callback(answer)
	}
	return &WriterOutput{
		FinalAnswer:     answer,
		EvidenceSummary: "summary",
		DraftConfidence: "high",
		TokenUsage:      types.TokenUsage{TotalTokens: 3},
	}, nil
}

func TestRunWritePhaseRetriesPlaceholderWithoutPublishingIt(t *testing.T) {
	writer := &answerSequenceWriter{answers: []string{"...", "complete answer"}}
	var chunks []string
	coordinator := &Coordinator{
		Writer: writer,
		TokenCallback: func(_ string, chunk string) {
			chunks = append(chunks, chunk)
		},
	}
	task := &types.Task{ID: "retry-placeholder", Goal: "goal", Status: types.StatusRunning}

	confidence, err := coordinator.runWritePhase(context.Background(), task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if writer.calls != 2 {
		t.Fatalf("writer calls = %d, want 2", writer.calls)
	}
	if confidence != "high" || task.FinalAnswer != "complete answer" {
		t.Fatalf("confidence=%q final_answer=%q", confidence, task.FinalAnswer)
	}
	if got := strings.Join(chunks, ""); got != "complete answer" {
		t.Fatalf("published chunks = %q", got)
	}
	if len(task.Trace) != 1 || task.Trace[0].TokenUsage.TotalTokens != 6 {
		t.Fatalf("trace usage = %+v", task.Trace)
	}
}

func TestRunWritePhaseFailsAfterRepeatedPlaceholder(t *testing.T) {
	writer := &answerSequenceWriter{answers: []string{"...", "…"}}
	var chunks []string
	coordinator := &Coordinator{
		Writer: writer,
		TokenCallback: func(_ string, chunk string) {
			chunks = append(chunks, chunk)
		},
	}
	task := &types.Task{ID: "reject-placeholder", Goal: "goal", Status: types.StatusRunning}

	confidence, err := coordinator.runWritePhase(context.Background(), task, nil)
	if err == nil {
		t.Fatal("expected placeholder generation to fail")
	}
	if confidence != "low" || task.Status != types.StatusFailed {
		t.Fatalf("confidence=%q status=%q", confidence, task.Status)
	}
	if task.FinalAnswer != invalidAnswerFallback {
		t.Fatalf("final_answer = %q", task.FinalAnswer)
	}
	if len(chunks) != 0 {
		t.Fatalf("placeholder chunks leaked: %#v", chunks)
	}
}

func TestApplyFinalVerificationOutputRejectsPlaceholder(t *testing.T) {
	coordinator := &Coordinator{}
	task := &types.Task{ID: "reviewed-placeholder", Goal: "goal"}
	output := &FinalVerificationOutput{FinalAnswer: "...", Supported: true, DraftConfidence: "high"}

	confidence := coordinator.applyFinalVerificationOutput(task, output, types.TokenUsage{}, 0)
	if confidence != "low" || output.Supported || task.FinalAnswer != invalidAnswerFallback {
		t.Fatalf("confidence=%q supported=%t final_answer=%q", confidence, output.Supported, task.FinalAnswer)
	}
}
