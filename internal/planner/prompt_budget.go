package planner

import (
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type promptBuildStats struct {
	UserPromptBytes     int
	MemoryAvailable     int
	MemoryIncluded      int
	MemoryOriginalBytes int
	MemoryIncludedBytes int
	MemoryBudgetBytes   int
	MemoryTruncated     bool
	TraceOriginalBytes  int
	TraceIncludedBytes  int
	TraceTruncated      bool
}

func buildMemoryPromptSection(task *types.Task) (string, promptBuildStats) {
	stats := promptBuildStats{MemoryAvailable: len(task.Memories)}
	for _, mem := range task.Memories {
		stats.MemoryOriginalBytes += len(mem.Goal) + len(mem.KeyFindings) + len(mem.FinalAnswer)
	}
	if len(task.Memories) == 0 {
		return "", stats
	}
	if !strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "prefetch") {
		stats.MemoryTruncated = true
		return "", stats
	}

	cfg := config.Get().RAG
	maxMemories := cfg.MaxPromptMemories
	if maxMemories <= 0 {
		maxMemories = 3
	}
	perMemoryBudget := cfg.MaxMemoryBytes
	if perMemoryBudget <= 0 {
		perMemoryBudget = 2500
	}
	totalBudget := effectiveMemoryPromptBudget(task, cfg.MaxMemoryPromptBytes)
	stats.MemoryBudgetBytes = totalBudget

	items := make([]string, 0, minInt(len(task.Memories), maxMemories))
	used := 0
	for _, mem := range task.Memories {
		if len(items) >= maxMemories || used >= totalBudget {
			stats.MemoryTruncated = true
			break
		}
		remaining := totalBudget - used
		itemBudget := minInt(perMemoryBudget, remaining)
		item, truncated := formatMemoryForPrompt(mem, len(items)+1, itemBudget)
		if item == "" {
			continue
		}
		items = append(items, item)
		used += len(item)
		stats.MemoryIncluded++
		stats.MemoryIncludedBytes += len(item)
		stats.MemoryTruncated = stats.MemoryTruncated || truncated
	}
	if stats.MemoryIncluded < stats.MemoryAvailable {
		stats.MemoryTruncated = true
	}
	if len(items) == 0 {
		return "", stats
	}
	return "\n\nRelated Historical Memories (Cross-task Knowledge Sharing):\n" + strings.Join(items, "\n\n"), stats
}

func effectiveMemoryPromptBudget(task *types.Task, configured int) int {
	if configured <= 0 {
		configured = 8000
	}
	if task == nil || task.TokenBudget <= 0 {
		return configured
	}
	used := 0
	for _, trace := range task.Trace {
		used += trace.TokenUsage.TotalTokens
	}
	remaining := task.TokenBudget - used
	if remaining <= 0 {
		return minInt(configured, 3000)
	}
	ratio := float64(remaining) / float64(task.TokenBudget)
	if ratio <= 0.25 {
		return minInt(configured, 3000)
	}
	if ratio <= 0.50 {
		return minInt(configured, 5000)
	}
	return configured
}

func formatMemoryForPrompt(mem types.Memory, index, budget int) (string, bool) {
	if budget <= 0 {
		return "", true
	}
	goalBudget := minInt(300, budget)
	answerBudget := minInt(700, maxInt(0, budget-goalBudget))
	findingsBudget := maxInt(0, budget-goalBudget-answerBudget)

	goal, goalTruncated := truncatePromptBytes(strings.TrimSpace(mem.Goal), goalBudget)
	answer := strings.TrimSpace(mem.FinalAnswer)
	if substantiallyDuplicates(mem.KeyFindings, answer) {
		answer = ""
	}
	answer, answerTruncated := truncatePromptBytes(answer, answerBudget)
	// Reassign unused goal/answer capacity to findings, the evidence-bearing field.
	findingsBudget += goalBudget - len(goal)
	findingsBudget += answerBudget - len(answer)
	findings, findingsTruncated := truncatePromptBytes(strings.TrimSpace(mem.KeyFindings), findingsBudget)

	var fields []string
	if goal != "" {
		fields = append(fields, "  * Goal: "+goal)
	}
	if findings != "" {
		fields = append(fields, "  * Key Findings:\n"+indentText(findings, "    "))
	}
	if answer != "" {
		fields = append(fields, "  * Final Answer: "+answer)
	}
	if len(fields) == 0 {
		return "", goalTruncated || findingsTruncated || answerTruncated
	}
	item := fmt.Sprintf("- Memory %d:\n%s", index, strings.Join(fields, "\n"))
	item, wrapperTruncated := truncatePromptBytes(item, budget)
	return item, goalTruncated || findingsTruncated || answerTruncated || wrapperTruncated
}

func substantiallyDuplicates(findings, answer string) bool {
	a := normalizePromptText(answer)
	if len([]rune(a)) < 30 {
		return false
	}
	f := normalizePromptText(findings)
	if len([]rune(f)) < 30 {
		return false
	}
	return strings.Contains(f, a) || strings.Contains(a, f)
}

func normalizePromptText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), ""))
}

func summarizeTraceWithStats(traces []types.StepTrace) (string, int, int, bool) {
	if len(traces) == 0 {
		return "No prior steps.", 0, 0, false
	}
	cfg := config.Get().LLM
	maxItems := cfg.PlannerTraceMaxItems
	if maxItems <= 0 {
		maxItems = 4
	}
	observationLimit := cfg.PlannerObservationMaxChars
	if observationLimit <= 0 {
		observationLimit = 800
	}
	evidenceLimit := cfg.PlannerEvidenceMaxItems
	if evidenceLimit <= 0 {
		evidenceLimit = 8
	}
	evidenceLineLimit := cfg.PlannerEvidenceLineMaxChars
	if evidenceLineLimit <= 0 {
		evidenceLineLimit = 300
	}
	traceLimit := cfg.PlannerTraceMaxChars
	if traceLimit <= 0 {
		traceLimit = 5000
	}

	original := summarizeTraceUnbounded(traces)
	start := maxInt(0, len(traces)-maxItems)
	truncated := start > 0
	var lines []string
	evidenceCount := 0
	for _, tr := range traces[start:] {
		observation, cut := truncatePromptRunes(tr.Observation, observationLimit)
		truncated = truncated || cut
		lines = append(lines, fmt.Sprintf("Step %d: action=%s, query=%s, observation=%s", tr.Step, tr.Action, tr.Query, observation))
		for _, ev := range tr.Evidence {
			for _, line := range ev.Lines {
				if evidenceCount >= evidenceLimit {
					truncated = true
					continue
				}
				line, cut = truncatePromptRunes(line, evidenceLineLimit)
				truncated = truncated || cut
				lines = append(lines, fmt.Sprintf("Evidence: %s :: %s", ev.Path, line))
				evidenceCount++
			}
		}
	}
	result := strings.Join(lines, "\n")
	result, cut := truncatePromptRunes(result, traceLimit)
	return result, len(original), len(result), truncated || cut
}

func summarizeTraceUnbounded(traces []types.StepTrace) string {
	var lines []string
	for _, tr := range traces {
		lines = append(lines, fmt.Sprintf("Step %d: action=%s, query=%s, observation=%s", tr.Step, tr.Action, tr.Query, tr.Observation))
		for _, ev := range tr.Evidence {
			for _, line := range ev.Lines {
				lines = append(lines, fmt.Sprintf("Evidence: %s :: %s", ev.Path, line))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func truncatePromptBytes(value string, limit int) (string, bool) {
	const suffix = "\n[truncated]"
	if limit <= 0 {
		return "", value != ""
	}
	if len(value) <= limit {
		return value, false
	}
	allowed := limit - len(suffix)
	if allowed < 0 {
		allowed = limit
	}
	cut := 0
	for index := range value {
		if index > allowed {
			break
		}
		cut = index
	}
	if limit < len(suffix) {
		return value[:cut], true
	}
	return value[:cut] + suffix, true
}

func truncatePromptRunes(value string, limit int) (string, bool) {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value, false
	}
	return string(runes[:limit]) + "... [truncated]", true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
