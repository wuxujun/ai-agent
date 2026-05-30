package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// SummarizeTask extracts key findings from a completed task's traces.
func SummarizeTask(task *types.Task) string {
	var findings []string
	for _, tr := range task.Trace {
		if tr.Observation == "" {
			continue
		}
		// Skip planner step itself as it doesn't represent factual evidence
		if tr.AgentRole == types.AgentRolePlanner || tr.Action == "plan" {
			continue
		}
		prefix := ""
		if tr.AgentRole != "" {
			prefix = fmt.Sprintf("[%s] ", tr.AgentRole)
		}
		findings = append(findings, fmt.Sprintf("- Step %d: %s%s (observation: %s)", tr.Step, prefix, tr.Action, tr.Observation))
	}
	if len(findings) == 0 {
		return "No detailed findings available."
	}
	return strings.Join(findings, "\n")
}

// CreateMemoryFromTask constructs a types.Memory object from a completed task.
// It generates a vector embedding of the goal and populates the timestamp.
func CreateMemoryFromTask(ctx context.Context, task *types.Task) (*types.Memory, error) {
	findings := SummarizeTask(task)

	// Combine goal and findings to create a rich text for embedding.
	// This helps with retrieving relevant memories based on both task goals and observations.
	richText := fmt.Sprintf("Goal: %s\nFindings: %s\nAnswer: %s", task.Goal, findings, task.FinalAnswer)

	emb, err := GetEmbedding(ctx, richText)
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding: %w", err)
	}

	return &types.Memory{
		ID:          "mem-" + task.ID,
		TaskID:      task.ID,
		Goal:        task.Goal,
		FinalAnswer: task.FinalAnswer,
		KeyFindings: findings,
		Timestamp:   time.Now(),
		Embedding:   emb,
	}, nil
}
