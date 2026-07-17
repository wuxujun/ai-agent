package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/types"
)

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

type VerifierAgent struct{}

func (v *VerifierAgent) Verify(ctx context.Context, goal, answer string, evidence []StepEvidence) (*VerificationResult, error) {
	cfg := LLMConfigForScene(config.LLMSceneAnswerVerifier)
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
	usage, err := callLLMJSON(ctx, cfg, "Check whether the draft answer is fully supported by the supplied evidence. Classify each issue as unsupported_claim, evidence_gap, or contradiction and identify the source when possible. Return JSON only.", prompt, schema, &result)
	result.TokenUsage = usage
	if err == nil {
		err = validateVerificationResult(&result)
	}
	return &result, err
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
