package policy

import (
	"context"
	"fmt"
	"strings"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type SafetyStage string

const (
	SafetyStageInput  SafetyStage = "input"
	SafetyStageOutput SafetyStage = "output"
)

type SafetyDecision struct {
	Allowed    bool
	SafeText   string
	Categories []string
	Reason     string
}

type SafetyGuard interface {
	Evaluate(ctx context.Context, stage SafetyStage, task *types.Task, text string) (*SafetyDecision, types.TokenUsage, error)
}

type LLMSafetyGuard struct {
	Scene string
}

func NewLLMSafetyGuard(scene string) *LLMSafetyGuard {
	return &LLMSafetyGuard{Scene: scene}
}

func (g *LLMSafetyGuard) Evaluate(ctx context.Context, stage SafetyStage, task *types.Task, text string) (*SafetyDecision, types.TokenUsage, error) {
	if stage != SafetyStageInput && stage != SafetyStageOutput {
		return nil, types.TokenUsage{}, fmt.Errorf("unsupported safety stage %q", stage)
	}
	var output struct {
		Allowed    bool     `json:"allowed"`
		SafeText   string   `json:"safe_text"`
		Categories []string `json:"categories"`
		Reason     string   `json:"reason"`
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"allowed":   map[string]any{"type": "boolean"},
			"safe_text": map[string]any{"type": "string"},
			"categories": map[string]any{
				"type": "array", "uniqueItems": true, "maxItems": 10,
				"items": map[string]any{"type": "string", "enum": []string{"privacy", "credentials", "violence", "self_harm", "sexual", "illegal", "malware", "hate", "harassment", "other"}},
			},
			"reason": map[string]any{"type": "string", "maxLength": 1000},
		},
		"required": []string{"allowed", "safe_text", "categories", "reason"},
	}
	goal := ""
	if task != nil {
		goal = task.Goal
	}
	prompt := fmt.Sprintf("Stage: %s\nTask goal: %s\n\nContent:\n%s", stage, goal, truncateSafetyText(text, 64000))
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(g.Scene), safetySystemPrompt, prompt, schema, &output)
	if err != nil {
		return nil, usage, err
	}
	output.SafeText = strings.TrimSpace(output.SafeText)
	if output.SafeText == "" {
		return nil, usage, fmt.Errorf("safety guard returned empty safe_text")
	}
	return &SafetyDecision{
		Allowed:    output.Allowed,
		SafeText:   output.SafeText,
		Categories: output.Categories,
		Reason:     strings.TrimSpace(output.Reason),
	}, usage, nil
}

const safetySystemPrompt = `Classify content safety and privacy risk. Block only content that directly facilitates credential theft, destructive malware, violent wrongdoing, sexual exploitation, targeted hate or harassment, or actionable self-harm. Allow benign coding, defensive security, analysis, summarization, and clearly fictional or educational discussion. For input, safe_text must be a brief refusal when blocked and otherwise preserve the original request. For output, remove exposed credentials and unnecessary private personal data; when allowed, preserve factual content and citation markers exactly. Set categories only for risks actually present. Return JSON only.`

func truncateSafetyText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
