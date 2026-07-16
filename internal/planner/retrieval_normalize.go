package planner

import (
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

type retrievalNormalization struct {
	Action            string
	OriginalCount     int
	PendingCount      int
	IncludedCount     int
	AllAlreadyFetched bool
}

// normalizeRetrievalActionParameters repairs deterministic retrieval argument
// issues before tool validation. Provider output must not fail an entire task
// merely because it repeated candidate IDs or returned more IDs than the
// configured per-fetch limit.
func normalizeRetrievalActionParameters(task *types.Task, decision *PlanDecision) []retrievalNormalization {
	if task == nil || decision == nil {
		return nil
	}
	limit := config.Get().RAG.JITFetchMaxItems
	if limit <= 0 {
		limit = 3
	}
	var changes []retrievalNormalization
	for i := range decision.Actions {
		action := &decision.Actions[i]
		if action.Action != "rag_fetch" && action.Action != "memory_get" {
			continue
		}
		rawIDs := retrievalIDs(action.Parameters["ids"])
		ids := uniqueRetrievalIDs(rawIDs)
		pending := tools.UnfetchedRetrievalIDs(task.ID, ids)
		selected := pending
		allFetched := len(ids) > 0 && len(pending) == 0
		if allFetched {
			// Keep a bounded valid action so the orchestrator can recognize that
			// every requested ID was already fetched and invoke the Finalizer.
			selected = ids
		}
		if len(selected) > limit {
			selected = selected[:limit]
		}
		if action.Parameters == nil {
			action.Parameters = make(map[string]any)
		}
		action.Parameters["ids"] = selected
		if len(rawIDs) != len(selected) || len(pending) != len(ids) || allFetched {
			changes = append(changes, retrievalNormalization{
				Action: action.Action, OriginalCount: len(rawIDs),
				PendingCount: len(pending), IncludedCount: len(selected), AllAlreadyFetched: allFetched,
			})
		}
	}
	return changes
}

func retrievalIDs(value any) []string {
	switch ids := value.(type) {
	case []string:
		return append([]string(nil), ids...)
	case []any:
		result := make([]string, 0, len(ids))
		for _, item := range ids {
			if id, ok := item.(string); ok {
				result = append(result, id)
			}
		}
		return result
	}
	return nil
}

func uniqueRetrievalIDs(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	result := make([]string, 0, len(raw))
	for _, id := range raw {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
