package multiagent

import (
	"context"
	"fmt"
	"strings"
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
	calls   int
	configs []llmcore.Config
}

type streamingDraftCaller struct{ sequentialFinalVerifierCaller }

func (c *streamingDraftCaller) CallJSONStream(ctx context.Context, cfg llmcore.Config, systemPrompt, userPrompt string, schema map[string]any, dest any, onChunk func(string)) (types.TokenUsage, error) {
	if _, ok := dest.(*VerificationDraft); !ok {
		return c.CallJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
	}
	onChunk(`{"evidence_summary":"private evidence","final_answer":"safe `)
	onChunk(`answer","draft_confidence":"high"}`)
	output := dest.(*VerificationDraft)
	output.FinalAnswer = "safe answer"
	output.EvidenceSummary = "private evidence"
	output.DraftConfidence = "high"
	return types.TokenUsage{TotalTokens: 4}, nil
}

func TestVerifierDraftStreamsOnlyFinalAnswer(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software_reviewed")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.Langfuse.Enabled = false }))
	caller := &streamingDraftCaller{}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	var chunks []string
	ctx = withAnswerTokenCallback(ctx, func(chunk string) { chunks = append(chunks, chunk) })
	draft, err := (&VerifierAgent{}).Draft(ctx, "goal", []StepEvidence{{StepID: "step-1", Observation: "evidence"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if draft.FinalAnswer != "safe answer" || strings.Join(chunks, "") != "safe answer" {
		t.Fatalf("draft=%+v chunks=%#v", draft, chunks)
	}
	if strings.Contains(strings.Join(chunks, ""), "private evidence") {
		t.Fatalf("non-answer structured field leaked: %#v", chunks)
	}
}

type bufferedCheckpointVerifier struct{ supported bool }

func (v bufferedCheckpointVerifier) Draft(ctx context.Context, _ string, _ []StepEvidence, _ []types.Memory) (*VerificationDraft, error) {
	if callback := answerTokenCallbackFromContext(ctx); callback != nil {
		callback("verified ")
		callback("answer")
	}
	return &VerificationDraft{FinalAnswer: "verified answer", EvidenceSummary: "evidence", DraftConfidence: "high"}, nil
}

func (v bufferedCheckpointVerifier) Verify(context.Context, string, string, []StepEvidence) (*VerificationResult, error) {
	result := &VerificationResult{Supported: v.supported}
	if !v.supported {
		result.Issues = []VerificationIssue{{Kind: "evidence_gap", Detail: "missing source"}}
	}
	return result, nil
}

func (v bufferedCheckpointVerifier) Finalize(context.Context, string, []StepEvidence, []types.Memory) (*FinalVerificationOutput, error) {
	return nil, fmt.Errorf("unexpected Finalize call")
}

func TestCoordinatorPublishesOnlyVerifiedAnswerChunks(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("supported_%t", supported), func(t *testing.T) {
			var chunks []string
			coordinator := &Coordinator{
				FinalVerifier: bufferedCheckpointVerifier{supported: supported},
				TokenCallback: func(_ string, chunk string) { chunks = append(chunks, chunk) },
			}
			task := &types.Task{ID: "verified-stream", Goal: "goal", Status: types.StatusRunning}
			_, gotSupported, err := coordinator.runVerifyPhase(context.Background(), task, nil, true, "")
			if err != nil {
				t.Fatal(err)
			}
			if gotSupported != supported {
				t.Fatalf("supported=%t", gotSupported)
			}
			if supported && strings.Join(chunks, "") != "verified answer" {
				t.Fatalf("verified chunks=%#v", chunks)
			}
			if !supported && len(chunks) != 0 {
				t.Fatalf("unsupported draft leaked chunks=%#v", chunks)
			}
		})
	}
}

type callbackWriter struct{ confidence string }

func (w callbackWriter) Write(ctx context.Context, _ string, _ []StepEvidence, _ []types.Memory) (*WriterOutput, error) {
	if callback := answerTokenCallbackFromContext(ctx); callback != nil {
		callback("writer ")
		callback("answer")
	}
	return &WriterOutput{FinalAnswer: "writer answer", EvidenceSummary: "summary", DraftConfidence: w.confidence}, nil
}

func TestCoordinatorPublishesOnlyAcceptedWriterChunks(t *testing.T) {
	for _, confidence := range []string{"high", "low"} {
		t.Run(confidence, func(t *testing.T) {
			var chunks []string
			coordinator := &Coordinator{
				Writer:        callbackWriter{confidence: confidence},
				TokenCallback: func(_ string, chunk string) { chunks = append(chunks, chunk) },
			}
			task := &types.Task{ID: "writer-stream", Goal: "goal", Status: types.StatusRunning}
			gotConfidence, err := coordinator.runWritePhase(context.Background(), task, nil)
			if err != nil || gotConfidence != confidence {
				t.Fatalf("confidence=%q err=%v", gotConfidence, err)
			}
			if confidence == "high" && strings.Join(chunks, "") != "writer answer" {
				t.Fatalf("accepted chunks=%#v", chunks)
			}
			if confidence == "low" && len(chunks) != 0 {
				t.Fatalf("low-confidence draft leaked chunks=%#v", chunks)
			}
		})
	}
}

func (c *sequentialFinalVerifierCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, _ string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.calls++
	c.configs = append(c.configs, cfg)
	switch output := dest.(type) {
	case *VerificationDraft:
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
	if caller.configs[0].Scene != config.LLMSceneMultiAgentWriter || caller.configs[1].Scene != config.LLMSceneAnswerVerifier {
		t.Fatalf("LLM scenes = [%q, %q]", caller.configs[0].Scene, caller.configs[1].Scene)
	}
	if output.Supported || output.DraftConfidence != "low" {
		t.Fatalf("output = %+v", output)
	}
	if output.TokenUsage != (types.TokenUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11}) {
		t.Fatalf("token usage = %+v", output.TokenUsage)
	}
}
