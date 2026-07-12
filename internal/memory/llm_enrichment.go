package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

func sceneConfigured(scene string) bool {
	_, ok := config.Get().LLM.Scenes[scene]
	return ok
}

func truncateLLMText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
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
	_, err := multiagent.CallLLMJSON(ctx, multiagent.LLMConfigForScene(config.LLMSceneMemorySummarizer), "Extract concise reusable factual findings. Preserve failures and decisions. Do not invent facts.", prompt, schema, &out)
	if err != nil || strings.TrimSpace(out.KeyFindings) == "" {
		return fallback
	}
	return truncateLLMText(out.KeyFindings, 4000)
}

func RewriteRAGQuery(ctx context.Context, query string) string {
	if !sceneConfigured(config.LLMSceneRAGQueryRewriter) {
		return query
	}
	var out struct {
		Query string `json:"query"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"query": map[string]any{"type": "string", "maxLength": 500}}, "required": []string{"query"}}
	_, err := multiagent.CallLLMJSON(ctx, multiagent.LLMConfigForScene(config.LLMSceneRAGQueryRewriter), "Rewrite the goal as one concise semantic retrieval query. Return JSON only.", query, schema, &out)
	if err != nil || strings.TrimSpace(out.Query) == "" {
		return query
	}
	return truncateLLMText(out.Query, 500)
}

func RerankMemories(ctx context.Context, query string, memories []types.Memory) []types.Memory {
	if !sceneConfigured(config.LLMSceneRAGReranker) || len(memories) < 2 {
		return memories
	}
	var candidates strings.Builder
	for _, mem := range memories {
		fmt.Fprintf(&candidates, "ID=%s Goal=%s Findings=%s Answer=%s\n", mem.ID, mem.Goal, mem.KeyFindings, mem.FinalAnswer)
	}
	var out struct {
		OrderedIDs []string `json:"ordered_ids"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ordered_ids": map[string]any{"type": "array", "minItems": len(memories), "maxItems": len(memories), "items": map[string]any{"type": "string", "maxLength": 200}}}, "required": []string{"ordered_ids"}}
	_, err := multiagent.CallLLMJSON(ctx, multiagent.LLMConfigForScene(config.LLMSceneRAGReranker), "Order candidate IDs from most to least relevant. Include every ID exactly once.", "Query: "+query+"\n"+candidates.String(), schema, &out)
	if err != nil || len(out.OrderedIDs) != len(memories) {
		return memories
	}
	byID := make(map[string]types.Memory, len(memories))
	for _, mem := range memories {
		byID[mem.ID] = mem
	}
	result := make([]types.Memory, 0, len(memories))
	for _, id := range out.OrderedIDs {
		mem, ok := byID[id]
		if !ok {
			return memories
		}
		result = append(result, mem)
		delete(byID, id)
	}
	return result
}
