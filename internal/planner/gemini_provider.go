package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

// geminiProvider is the LLMProvider implementation for Google Gemini via the
// google.golang.org/genai SDK. It plugs into the same providerRegistry as the
// HTTP-based providers — PlanNext no longer special-cases Gemini.
//
// The genai SDK does not produce *http.Request objects directly (it owns its
// own transport), which is why the LLMProvider interface centres on the
// higher-level Plan method rather than the HTTP-shaped BuildRequest of the
// earlier design.
type geminiProvider struct{}

func (p *geminiProvider) Name() ProviderType { return ProviderGemini }

func (p *geminiProvider) Plan(ctx context.Context, req PlanRequest, onChunk func(string)) (string, types.TokenUsage, error) {
	client, err := GetGeminiClient(req.APIKey, req.BaseURL)
	if err != nil {
		return "", types.TokenUsage{}, fmt.Errorf("get gemini client: %w", err)
	}

	contents := []*genai.Content{
		{
			Role:  "user",
			Parts: []*genai.Part{{Text: req.UserPrompt}},
		},
	}

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: req.SystemPrompt}},
		},
		ResponseMIMEType: "application/json",
		ResponseSchema:   PlannerDecisionGenAISchema(),
	}

	log.Info("Gemini planner request",
		"model", req.Model,
		"base_url", req.BaseURL,
		"response_mime_type", cfg.ResponseMIMEType,
		"system_prompt_len", len(req.SystemPrompt),
		"system_prompt", req.SystemPrompt,
		"user_prompt_len", len(req.UserPrompt),
		"user_prompt", req.UserPrompt,
	)

	iter := client.Models.GenerateContentStream(ctx, req.Model, contents, cfg)

	var textBuf strings.Builder
	var usage types.TokenUsage

	for resp, err := range iter {
		if err != nil {
			return "", usage, err
		}
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil && len(resp.Candidates[0].Content.Parts) > 0 {
			for _, part := range resp.Candidates[0].Content.Parts {
				chunk := part.Text
				if chunk != "" {
					textBuf.WriteString(chunk)
					if onChunk != nil {
						onChunk(chunk)
					}
				}
			}
		}
		if resp.UsageMetadata != nil {
			usage.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
			usage.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
			usage.TotalTokens = int(resp.UsageMetadata.TotalTokenCount)
		}
	}

	responseText := textBuf.String()
	log.Info("Gemini planner response",
		"model", req.Model,
		"response_len", len(responseText),
		"response", responseText,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"total_tokens", usage.TotalTokens,
	)

	return responseText, usage, nil
}
