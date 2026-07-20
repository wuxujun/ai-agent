package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
)

const verifierSystemPrompt = `Check whether the draft answer is fully supported by the supplied evidence. Classify each issue as unsupported_claim, evidence_gap, or contradiction and identify the source when possible. Return JSON only.`

const finalVerifierSystemPrompt = `You are the final verifier in a plan-review-execute-verify workflow. Produce the final answer using only the supplied execution evidence, then independently check every material claim against that evidence.

Rules:
1. final_answer must be complete and self-contained.
2. Set supported=false when any material claim lacks evidence or contradicts evidence.
3. Classify issues as unsupported_claim, evidence_gap, or contradiction.
4. Set draft_confidence to low when supported=false, otherwise high or medium according to evidence coverage.
5. evidence_summary must briefly identify the evidence used.
6. Return JSON only.`

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

type VerifierAgent struct{}

func (v *VerifierAgent) Verify(ctx context.Context, goal, answer string, evidence []StepEvidence) (*VerificationResult, error) {
	cfg := LLMConfigForScene(config.LLMSceneAnswerVerifier)
	agentCfg := GetTeamsConfig().GetActiveTeam().Verifier
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
	systemPrompt := resolveAgentPrompt(ctx, agentCfg, "multiagent_verifier_prompt", verifierSystemPrompt)
	usage, err := callLLMJSON(ctx, cfg, systemPrompt, prompt, schema, &result)
	result.TokenUsage = usage
	if err == nil {
		err = validateVerificationResult(&result)
	}
	return &result, err
}

func (v *VerifierAgent) Finalize(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*FinalVerificationOutput, error) {
	agentCfg := GetTeamsConfig().GetActiveTeam().Verifier
	cfg := GetLLMConfig(agentCfg, config.LLMSceneAnswerVerifier)
	systemPrompt := resolveAgentPrompt(ctx, agentCfg, "multiagent_final_verifier_prompt", finalVerifierSystemPrompt)
	prompt := (&WriterAgent{}).buildPrompt(goal, evidence, memories)
	var output FinalVerificationOutput
	usage, err := callLLMJSON(ctx, cfg, systemPrompt, prompt, finalVerifierSchema(), &output)
	output.TokenUsage = usage
	if err != nil {
		return &output, err
	}
	verification := &VerificationResult{Supported: output.Supported, Issues: output.Issues}
	if err := validateVerificationResult(verification); err != nil {
		return &output, err
	}
	output.Issues = verification.Issues
	if strings.TrimSpace(output.FinalAnswer) == "" || strings.TrimSpace(output.EvidenceSummary) == "" {
		return &output, fmt.Errorf("final verifier returned an incomplete answer")
	}
	return &output, nil
}

func finalVerifierSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"final_answer":     map[string]any{"type": "string"},
			"evidence_summary": map[string]any{"type": "string"},
			"draft_confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
			"supported":        map[string]any{"type": "boolean"},
			"issues": map[string]any{
				"type": "array", "maxItems": 10,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"kind":      map[string]any{"type": "string", "enum": []string{"unsupported_claim", "evidence_gap", "contradiction"}},
						"detail":    map[string]any{"type": "string", "maxLength": 500},
						"source_id": map[string]any{"type": "string", "maxLength": 200},
					},
					"required": []string{"kind", "detail", "source_id"},
				},
			},
		},
		"required": []string{"final_answer", "evidence_summary", "draft_confidence", "supported", "issues"},
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
