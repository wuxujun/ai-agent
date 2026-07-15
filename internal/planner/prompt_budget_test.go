package planner

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestBuildUserPromptAppliesMemoryBudgetsWithoutMutatingTask(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.MaxPromptMemories = 3
		cfg.RAG.MaxMemoryBytes = 2500
		cfg.RAG.MaxMemoryPromptBytes = 8000
	}))
	large := strings.Repeat("数学顾问资料", 2000)
	task := &types.Task{
		Goal:       "汇总数学科学术顾问信息",
		MaxSteps:   5,
		ToolBudget: 10,
		Memories: []types.Memory{
			{ID: "high", Goal: "最高相关", KeyFindings: large},
			{ID: "second", Goal: "第二相关", KeyFindings: large},
			{ID: "third", Goal: "第三相关", KeyFindings: large},
			{ID: "fourth", Goal: "不应进入", KeyFindings: large},
		},
	}

	prompt, stats := buildUserPromptWithStats(task)
	if stats.MemoryIncluded != 3 || stats.MemoryIncludedBytes > 8000 || !stats.MemoryTruncated {
		t.Fatalf("unexpected memory prompt stats: %+v", stats)
	}
	if !strings.Contains(prompt, "最高相关") || strings.Contains(prompt, "不应进入") {
		t.Fatalf("prompt did not preserve rerank order/limit: %s", prompt)
	}
	if !utf8.ValidString(prompt) {
		t.Fatal("budgeted prompt is not valid UTF-8")
	}
	if task.Memories[0].KeyFindings != large {
		t.Fatal("prompt budgeting mutated the persisted task memory")
	}
}

func TestEffectiveMemoryPromptBudgetShrinksWithRemainingTokens(t *testing.T) {
	task := &types.Task{TokenBudget: 20000, Trace: []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 11000}}}}
	if got := effectiveMemoryPromptBudget(task, 8000); got != 5000 {
		t.Fatalf("budget at 45%% remaining = %d, want 5000", got)
	}
	task.Trace[0].TokenUsage.TotalTokens = 16000
	if got := effectiveMemoryPromptBudget(task, 8000); got != 3000 {
		t.Fatalf("budget at 20%% remaining = %d, want 3000", got)
	}
}

func TestSummarizeTraceAppliesEvidenceAndUnicodeLimits(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.PlannerTraceMaxItems = 1
		cfg.LLM.PlannerObservationMaxChars = 10
		cfg.LLM.PlannerEvidenceMaxItems = 1
		cfg.LLM.PlannerEvidenceLineMaxChars = 5
		cfg.LLM.PlannerTraceMaxChars = 200
	}))
	traces := []types.StepTrace{
		{Step: 1, Observation: "old"},
		{Step: 2, Action: "search_text", Observation: strings.Repeat("数学", 20), Evidence: []types.Evidence{{Path: "a.md", Lines: []string{"第一条很长证据", "第二条证据"}}}},
	}
	result, originalBytes, includedBytes, truncated := summarizeTraceWithStats(traces)
	if !truncated || originalBytes <= includedBytes || strings.Contains(result, "old") || strings.Contains(result, "第二条证据") {
		t.Fatalf("trace budget not applied: original=%d included=%d result=%q", originalBytes, includedBytes, result)
	}
	if !utf8.ValidString(result) {
		t.Fatal("trace truncation produced invalid UTF-8")
	}
}

func TestMemoryPromptOmitsDuplicatedFinalAnswer(t *testing.T) {
	answer := strings.Repeat("same factual answer ", 3)
	item, _ := formatMemoryForPrompt(types.Memory{KeyFindings: "prefix " + answer + " suffix", FinalAnswer: answer}, 1, 2500)
	if strings.Contains(item, "Final Answer:") {
		t.Fatalf("duplicated final answer was included: %s", item)
	}
}

func TestMemoryPromptPreservesAnswerWhenFindingsAreEmpty(t *testing.T) {
	answer := strings.Repeat("standalone factual answer ", 3)
	item, _ := formatMemoryForPrompt(types.Memory{FinalAnswer: answer}, 1, 2500)
	if !strings.Contains(item, "Final Answer:") || !strings.Contains(item, "standalone factual answer") {
		t.Fatalf("standalone final answer was dropped: %s", item)
	}
}
