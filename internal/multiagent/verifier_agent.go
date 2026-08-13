package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
)

const verifierSystemPrompt = `Check whether the draft answer is fully supported by the supplied evidence. Classify each issue as unsupported_claim, evidence_gap, or contradiction and identify the source when possible. Return JSON only.`

const finalVerifierDraftSystemPrompt = `You are the answer drafting stage of the verifier role in a plan-review-execute-verify workflow. Produce a candidate final answer using only the supplied execution evidence. Do not judge your own answer; an independent verification call will do that.

Rules:
1. final_answer must be complete and self-contained.
2. draft_confidence must reflect evidence coverage.
3. evidence_summary must briefly identify the evidence used.
4. Return JSON only.`

type VerificationIssue struct {
	Kind     string `json:"kind"`
	Detail   string `json:"detail"`
	SourceID string `json:"source_id,omitempty"`
}

type VerificationResult struct {
	Supported  bool                `json:"supported"`
	Issues     []VerificationIssue `json:"issues"`
	TokenUsage types.TokenUsage    `json:"token_usage,omitempty"`
}

type AnswerVerifier interface {
	Verify(ctx context.Context, goal, answer string, evidence []StepEvidence) (*VerificationResult, error)
}

type FinalVerificationOutput struct {
	FinalAnswer     string              `json:"final_answer"`
	EvidenceSummary string              `json:"evidence_summary"`
	DraftConfidence string              `json:"draft_confidence"`
	Supported       bool                `json:"supported"`
	Issues          []VerificationIssue `json:"issues"`
	TokenUsage      types.TokenUsage    `json:"token_usage,omitempty"`
}

type VerificationDraft struct {
	FinalAnswer     string           `json:"final_answer"`
	EvidenceSummary string           `json:"evidence_summary"`
	DraftConfidence string           `json:"draft_confidence"`
	TokenUsage      types.TokenUsage `json:"token_usage,omitempty"`
}

func (o *FinalVerificationOutput) resolvedDraftConfidence() string {
	if o == nil || !o.Supported {
		return "low"
	}
	switch value := strings.ToLower(strings.TrimSpace(o.DraftConfidence)); value {
	case "high", "medium", "low":
		return value
	default:
		return "low"
	}
}

type FinalVerifier interface {
	Finalize(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*FinalVerificationOutput, error)
}

// CheckpointFinalVerifier exposes the two independent verifier stages so the
// coordinator can persist a generated draft before starting verification.
type CheckpointFinalVerifier interface {
	FinalVerifier
	AnswerVerifier
	Draft(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*VerificationDraft, error)
}

type VerifierAgent struct{}

func (v *VerifierAgent) Verify(ctx context.Context, goal, answer string, evidence []StepEvidence) (*VerificationResult, error) {
	cfg := LLMConfigForScene(config.LLMSceneAnswerVerifier)
	agentCfg := teamConfigFromContext(ctx).Team.Verifier
	if agentCfg.Provider != "" || agentCfg.Model != "" || agentCfg.LLMScene != "" {
		cfg = GetLLMConfig(agentCfg, config.LLMSceneAnswerVerifier)
	}
	prompt := fmt.Sprintf("Goal: %s\nAnswer: %s\nEvidence: %+v", goal, answer, evidence)
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"supported": map[string]any{"type": "boolean"},
			"issues": map[string]any{
				"type": "array", "maxItems": 10,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"kind":      map[string]any{"type": "string", "enum": []string{"unsupported_claim", "evidence_gap", "contradiction"}},
						"detail":    map[string]any{"type": "string", "maxLength": 500},
						"source_id": map[string]any{"type": "string", "maxLength": 200},
					},
					"required": []string{"kind", "detail"},
				},
			},
		},
		"required": []string{"supported", "issues"},
	}
	var result VerificationResult
	resolvedPrompt, promptErr := resolveAgentPromptDetailsForTask(ctx, agentCfg, "multiagent_verifier_prompt", verifierSystemPrompt)
	if promptErr != nil {
		return nil, fmt.Errorf("resolve VerifierAgent prompt: %w", promptErr)
	}
	callCtx := llmcore.WithPromptBinding(ctx, resolvedPrompt.Binding)
	systemPrompt := resolvedPrompt.Content
	usage, err := callLLMJSON(callCtx, cfg, systemPrompt, prompt, schema, &result)
	result.TokenUsage = usage
	if err == nil {
		err = validateVerificationResult(&result)
	}
	return &result, err
}

func (v *VerifierAgent) Draft(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*VerificationDraft, error) {
	agentCfg := teamConfigFromContext(ctx).Team.Verifier
	draftCfg := draftAgentConfig(agentCfg)
	cfg := GetLLMConfig(draftCfg, config.LLMSceneMultiAgentWriter)
	resolvedPrompt, promptErr := resolveAgentPromptDetailsForTask(ctx, draftCfg, "multiagent_final_verifier_draft_prompt", finalVerifierDraftSystemPrompt)
	if promptErr != nil {
		return nil, fmt.Errorf("resolve VerifierAgent draft prompt: %w", promptErr)
	}
	callCtx := llmcore.WithPromptBinding(ctx, resolvedPrompt.Binding)
	systemPrompt := resolvedPrompt.Content
	prompt := (&WriterAgent{}).buildPrompt(goal, evidence, memories)
	if answerRegenerationRequested(ctx) {
		prompt += "\n\nThe previous generation was rejected as empty or placeholder content. Regenerate a complete, substantive final_answer."
	}
	var candidate VerificationDraft
	var draftUsage types.TokenUsage
	var err error
	callback := answerTokenCallbackFromContext(callCtx)
	if callback != nil {
		answerStream := llmcore.NewJSONStringFieldStream("final_answer", callback)
		draftUsage, err = callLLMJSONStream(callCtx, cfg, systemPrompt, prompt, finalAnswerCandidateSchema(), &candidate, answerStream.Write)
	} else {
		draftUsage, err = callLLMJSON(callCtx, cfg, systemPrompt, prompt, finalAnswerCandidateSchema(), &candidate)
	}
	candidate.TokenUsage = draftUsage
	if err != nil {
		return nil, fmt.Errorf("generate verification candidate: %w", err)
	}
	if strings.TrimSpace(candidate.FinalAnswer) == "" || strings.TrimSpace(candidate.EvidenceSummary) == "" {
		return nil, fmt.Errorf("final verifier returned an incomplete candidate answer")
	}
	return &candidate, nil
}

func (v *VerifierAgent) Finalize(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*FinalVerificationOutput, error) {
	candidate, err := v.Draft(ctx, goal, evidence, memories)
	if err != nil {
		return nil, err
	}

	verification, err := v.Verify(ctx, goal, candidate.FinalAnswer, evidence)
	if err != nil {
		return nil, fmt.Errorf("verify candidate answer: %w", err)
	}
	output := &FinalVerificationOutput{
		FinalAnswer:     candidate.FinalAnswer,
		EvidenceSummary: candidate.EvidenceSummary,
		DraftConfidence: candidate.DraftConfidence,
		Supported:       verification.Supported,
		Issues:          verification.Issues,
		TokenUsage:      candidate.TokenUsage,
	}
	addMultiAgentUsage(&output.TokenUsage, verification.TokenUsage)
	if !output.Supported {
		output.DraftConfidence = "low"
	}
	return output, nil
}

func finalAnswerCandidateSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"final_answer":     map[string]any{"type": "string"},
			"evidence_summary": map[string]any{"type": "string"},
			"draft_confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		},
		"required": []string{"final_answer", "evidence_summary", "draft_confidence"},
	}
}

func validateVerificationResult(result *VerificationResult) error {
	if result == nil {
		return fmt.Errorf("answer verifier returned no result")
	}
	if result.Supported && len(result.Issues) > 0 {
		return fmt.Errorf("answer verifier returned issues for a supported answer")
	}
	if !result.Supported && len(result.Issues) == 0 {
		return fmt.Errorf("answer verifier returned unsupported without issues")
	}
	for i := range result.Issues {
		issue := &result.Issues[i]
		switch issue.Kind {
		case "unsupported_claim", "evidence_gap", "contradiction":
		default:
			return fmt.Errorf("answer verifier returned invalid issue kind %q", issue.Kind)
		}
		issue.Detail = truncateVerificationField(sanitize.Secrets(strings.TrimSpace(issue.Detail)), 500)
		issue.SourceID = truncateVerificationField(sanitize.Secrets(strings.TrimSpace(issue.SourceID)), 200)
		if issue.Detail == "" {
			return fmt.Errorf("answer verifier returned issue without detail")
		}
	}
	return nil
}

func truncateVerificationField(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
