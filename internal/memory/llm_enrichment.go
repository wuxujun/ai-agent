package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func sceneConfigured(scene string) bool {
	_, ok := config.Get().LLM.Scenes[scene]
	return ok
}

func truncateLLMText(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func summarizeMemoryWithLLM(ctx context.Context, task *types.Task, fallback string) string {
	if !sceneConfigured(config.LLMSceneMemorySummarizer) {
		return fallback
	}
	var out struct {
		KeyFindings string `json:"key_findings"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"key_findings": map[string]any{"type": "string", "maxLength": 4000}}, "required": []string{"key_findings"}}
	prompt := fmt.Sprintf("Goal: %s\nAnswer: %s\nTrace findings:\n%s", task.Goal, task.FinalAnswer, fallback)
	_, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(config.LLMSceneMemorySummarizer), "Extract concise reusable factual findings. Preserve failures and decisions. Do not invent facts.", prompt, schema, &out)
	if err != nil || strings.TrimSpace(out.KeyFindings) == "" {
		return fallback
	}
	return truncateLLMText(out.KeyFindings, 4000)
}

func RewriteRAGQuery(ctx context.Context, query string) (string, types.TokenUsage) {
	if !sceneConfigured(config.LLMSceneRAGQueryRewriter) {
		return query, types.TokenUsage{}
	}
	var out struct {
		Query string `json:"query"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"query": map[string]any{"type": "string", "maxLength": 500}}, "required": []string{"query"}}
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(config.LLMSceneRAGQueryRewriter), "Rewrite the goal as one concise semantic retrieval query. Return JSON only.", query, schema, &out)
	if err != nil || strings.TrimSpace(out.Query) == "" {
		return query, usage
	}
	return truncateLLMText(out.Query, 500), usage
}

func RerankMemories(ctx context.Context, query string, memories []types.Memory) ([]types.Memory, types.TokenUsage) {
	if !sceneConfigured(config.LLMSceneRAGReranker) || len(memories) < 2 {
		return memories, types.TokenUsage{}
	}
	var candidates strings.Builder
	for _, mem := range memories {
		fmt.Fprintf(&candidates, "ID=%s Goal=%s Findings=%s Answer=%s\n", truncateLLMText(mem.ID, 200), truncateLLMText(mem.Goal, 500), truncateLLMText(mem.KeyFindings, 2000), truncateLLMText(mem.FinalAnswer, 2000))
	}
	var out struct {
		OrderedIDs []string `json:"ordered_ids"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ordered_ids": map[string]any{"type": "array", "minItems": len(memories), "maxItems": len(memories), "items": map[string]any{"type": "string", "maxLength": 200}}}, "required": []string{"ordered_ids"}}
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(config.LLMSceneRAGReranker), "Order candidate IDs from most to least relevant. Include every ID exactly once.", "Query: "+query+"\n"+truncateLLMText(candidates.String(), 24000), schema, &out)
	if err != nil || len(out.OrderedIDs) != len(memories) {
		return memories, usage
	}
	byID := make(map[string]types.Memory, len(memories))
	for _, mem := range memories {
		byID[mem.ID] = mem
	}
	result := make([]types.Memory, 0, len(memories))
	for _, id := range out.OrderedIDs {
		mem, ok := byID[id]
		if !ok {
			return memories, usage
		}
		result = append(result, mem)
		delete(byID, id)
	}
	return result, usage
}
