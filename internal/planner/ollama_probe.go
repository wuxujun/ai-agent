package planner

import (
	"context"
	"github.com/wuxujun/ai-agent/internal/llm"
)

// Compatibility aliases keep callers on the shared provider implementation.
type OllamaModelList = llm.OllamaModelList

func ResolveOllamaBaseURL(rawURL string) string { return llm.ResolveOllamaBaseURL(rawURL) }
func ProbeOllama(ctx context.Context, baseURL, modelName string) error {
	return llm.ProbeOllama(ctx, baseURL, modelName)
}
