package vision

import (
	"context"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type Analysis struct {
	Description  string   `json:"description"`
	DetectedText string   `json:"detected_text"`
	Objects      []string `json:"objects"`
	Warnings     []string `json:"warnings"`
}

func Analyze(ctx context.Context, mimeType string, data []byte, prompt string) (Analysis, types.TokenUsage, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = "Describe the image, extract visible text, identify important objects, and note uncertainty."
	}
	var result Analysis
	usage, err := llm.CallVisionJSON(ctx, llm.ConfigForScene(config.LLMSceneVisionAnalyzer),
		"Analyze the supplied image. Return factual JSON only. Do not infer details that are not visually supported.",
		prompt, llm.VisionInput{MIMEType: mimeType, Data: data}, analysisSchema(), &result)
	return result, usage, err
}

func analysisSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"description":   map[string]any{"type": "string"},
			"detected_text": map[string]any{"type": "string"},
			"objects":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"warnings":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"description", "detected_text", "objects", "warnings"},
	}
}
