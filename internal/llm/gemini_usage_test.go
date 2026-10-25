package llm

import (
	"math"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

func TestGeminiTokenUsage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata *genai.GenerateContentResponseUsageMetadata
		want     types.TokenUsage
	}{
		{"missing", nil, types.TokenUsage{}},
		{"ordinary and cached input", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CachedContentTokenCount: 2, CandidatesTokenCount: 7, TotalTokenCount: 12}, types.TokenUsage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}},
		{"thinking and tool input", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CachedContentTokenCount: 2, ToolUsePromptTokenCount: 3, CandidatesTokenCount: 7, ThoughtsTokenCount: 11, TotalTokenCount: 26}, types.TokenUsage{PromptTokens: 8, CompletionTokens: 18, TotalTokens: 26}},
		{"missing total preserved", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 7, ThoughtsTokenCount: 11}, types.TokenUsage{PromptTokens: 5, CompletionTokens: 18}},
		{"inconsistent total preserved", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 7, ThoughtsTokenCount: 11, TotalTokenCount: 12}, types.TokenUsage{PromptTokens: 5, CompletionTokens: 18, TotalTokens: 12}},
		{"sum after int conversion", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: math.MaxInt32, ToolUsePromptTokenCount: 1, CandidatesTokenCount: math.MaxInt32, ThoughtsTokenCount: 1}, types.TokenUsage{PromptTokens: int(math.MaxInt32) + 1, CompletionTokens: int(math.MaxInt32) + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GeminiTokenUsage(tc.metadata); got != tc.want {
				t.Fatalf("usage=%+v want=%+v", got, tc.want)
			}
		})
	}
}
