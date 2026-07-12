package multiagent

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type VerificationResult struct {
	Supported  bool             `json:"supported"`
	Issues     []string         `json:"issues"`
	TokenUsage types.TokenUsage `json:"token_usage,omitempty"`
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
			"issues":    map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{"type": "string", "maxLength": 500}},
		},
		"required": []string{"supported", "issues"},
	}
	var result VerificationResult
	usage, err := callLLMJSON(ctx, cfg, "Check whether the answer is fully supported by the supplied evidence. Report concise factual gaps or contradictions. Return JSON only.", prompt, schema, &result)
	result.TokenUsage = usage
	return &result, err
}
