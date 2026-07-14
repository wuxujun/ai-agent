package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type conflictCaller struct {
	config llmcore.Config
	prompt string
	result map[string]any
}

func (c *conflictCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config = cfg
	c.prompt = prompt
	payload, _ := json.Marshal(c.result)
	if err := json.Unmarshal(payload, dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18}, nil
}

func TestLLMMemoryConflictResolverFiltersContradictedMemory(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneMemoryConflictResolver: {Provider: "openai-responses", Model: "resolver"},
		}
	}))
	caller := &conflictCaller{result: map[string]any{
		"assessments": []map[string]any{
			{"memory_index": 1, "status": "drop", "reason": "contradicted by current evidence"},
			{"memory_index": 2, "status": "keep", "reason": "matches current evidence"},
		},
		"conflicts": []map[string]any{{"memory_indexes": []int{1, 2}, "claim": "feature state", "resolution": "current evidence supports memory 2"}},
	}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	task := &types.Task{
		Goal: "check feature",
		Memories: []types.Memory{
			{ID: "old", FinalAnswer: "disabled", Timestamp: time.Unix(1, 0)},
			{ID: "new", FinalAnswer: "enabled", Timestamp: time.Unix(2, 0)},
		},
		Trace: []types.StepTrace{{Evidence: []types.Evidence{{Path: "config.yaml", Lines: []string{"enabled: true"}}}}},
	}
	resolution, usage, err := NewLLMMemoryConflictResolver(config.LLMSceneMemoryConflictResolver).Resolve(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Memories) != 1 || resolution.Memories[0].ID != "new" || resolution.Dropped != 1 || resolution.ConflictCount != 1 || usage.TotalTokens != 18 {
		t.Fatalf("resolution=%+v usage=%+v", resolution, usage)
	}
	if caller.config.Scene != config.LLMSceneMemoryConflictResolver || !strings.Contains(caller.prompt, "enabled: true") {
		t.Fatalf("config=%+v prompt=%q", caller.config, caller.prompt)
	}
}

func TestLLMMemoryConflictResolverRejectsDuplicateAssessment(t *testing.T) {
	caller := &conflictCaller{result: map[string]any{
		"assessments": []map[string]any{
			{"memory_index": 1, "status": "keep", "reason": "first"},
			{"memory_index": 1, "status": "drop", "reason": "duplicate"},
		},
		"conflicts": []any{},
	}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	task := &types.Task{Memories: []types.Memory{{ID: "one"}, {ID: "two"}}}
	if _, _, err := NewLLMMemoryConflictResolver("resolver").Resolve(ctx, task); err == nil || !strings.Contains(err.Error(), "duplicate index") {
		t.Fatalf("duplicate assessment error = %v", err)
	}
}

func TestConflictEvidenceCountIgnoresEmptyEvidence(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{
		{Evidence: []types.Evidence{{Lines: []string{""}}, {Lines: []string{"fact"}}}},
		{Evidence: []types.Evidence{{Lines: []string{"other"}}}},
	}}
	if got := ConflictEvidenceCount(task); got != 2 {
		t.Fatalf("evidence count = %d, want 2", got)
	}
}

func TestCurrentEvidenceCatalogKeepsMostRecentTwenty(t *testing.T) {
	task := &types.Task{}
	for i := 0; i < 25; i++ {
		task.Trace = append(task.Trace, types.StepTrace{Evidence: []types.Evidence{{Path: fmt.Sprintf("source-%d", i), Lines: []string{"fact"}}}})
	}
	catalog := currentEvidenceCatalog(task)
	if len(catalog) != 20 || catalog[0].Source != "source-5" || catalog[19].Source != "source-24" || catalog[0].Index != 1 || catalog[19].Index != 20 {
		t.Fatalf("recent evidence catalog = %+v", catalog)
	}
}
