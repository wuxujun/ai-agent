package planner

import (
	"context"
	"net/http"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"google.golang.org/genai"
)

var geminiHTTPClient *http.Client

// GetGeminiClient remains as a compatibility wrapper for planner providers.
func GetGeminiClient(apiKey, baseURL string) (*genai.Client, error) {
	if geminiHTTPClient != nil {
		cfg := &genai.ClientConfig{APIKey: apiKey, HTTPClient: geminiHTTPClient}
		if baseURL != "" {
			cfg.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
		}
		return genai.NewClient(context.Background(), cfg)
	}
	return llmcore.GetGeminiClient(apiKey, baseURL)
}
