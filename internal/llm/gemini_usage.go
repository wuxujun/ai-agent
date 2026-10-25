package llm

import (
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

// GeminiTokenUsage maps the SDK's documented usage breakdown to the runtime's
// input/output categories. Cached tokens are already included in prompt tokens;
// tool results are additional input, and generated thoughts are output. Keep
// the provider's total unchanged so inconsistent or missing evidence remains
// visible to callers that validate usage before accepting results.
func GeminiTokenUsage(metadata *genai.GenerateContentResponseUsageMetadata) types.TokenUsage {
	if metadata == nil {
		return types.TokenUsage{}
	}
	return types.TokenUsage{
		PromptTokens:     int(metadata.PromptTokenCount) + int(metadata.ToolUsePromptTokenCount),
		CompletionTokens: int(metadata.CandidatesTokenCount) + int(metadata.ThoughtsTokenCount),
		TotalTokens:      int(metadata.TotalTokenCount),
	}
}
