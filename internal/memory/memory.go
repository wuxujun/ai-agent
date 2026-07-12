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
	findings = summarizeMemoryWithLLM(ctx, task, findings)

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

// DeduplicateMemories filters out duplicate or highly redundant memories.
// It filters by duplicate ID, TaskID, and exact normalized matches of Goal or FinalAnswer,
// as well as by semantic similarity based on their vector embeddings (if available).
func DeduplicateMemories(memories []types.Memory) []types.Memory {
	if len(memories) <= 1 {
		return memories
	}

	seenIDs := make(map[string]bool)
	seenTaskIDs := make(map[string]bool)
	seenGoals := make(map[string]bool)
	seenAnswers := make(map[string]bool)

	// Threshold for semantic duplicate detection (cosine similarity).
	// Memories with cosine similarity higher than this value are considered redundant.
	const semanticThreshold = 0.85

	var deduped []types.Memory

	for _, mem := range memories {
		// Normalize fields for comparison
		id := strings.TrimSpace(mem.ID)
		taskID := strings.TrimSpace(mem.TaskID)
		goal := strings.ToLower(strings.TrimSpace(mem.Goal))
		answer := strings.ToLower(strings.TrimSpace(mem.FinalAnswer))

		if id != "" && seenIDs[id] {
			continue
		}
		if taskID != "" && seenTaskIDs[taskID] {
			continue
		}
		if goal != "" && seenGoals[goal] {
			continue
		}
		if answer != "" && seenAnswers[answer] {
			continue
		}

		// Check semantic similarity against already accepted memories
		isSemanticDuplicate := false
		if len(mem.Embedding) > 0 {
			for _, other := range deduped {
				if len(other.Embedding) > 0 {
					sim := CosineSimilarity(mem.Embedding, other.Embedding)
					if sim >= semanticThreshold {
						log.Debug("deduplicating semantically similar memory", "id", mem.ID, "other_id", other.ID, "similarity", sim)
						isSemanticDuplicate = true
						break
					}
				}
			}
		}

		if isSemanticDuplicate {
			continue
		}

		// Mark as seen
		if id != "" {
			seenIDs[id] = true
		}
		if taskID != "" {
			seenTaskIDs[taskID] = true
		}
		if goal != "" {
			seenGoals[goal] = true
		}
		if answer != "" {
			seenAnswers[answer] = true
		}

		deduped = append(deduped, mem)
	}

	return deduped
}
