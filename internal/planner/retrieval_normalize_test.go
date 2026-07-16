package planner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestNormalizeRetrievalActionParametersDeduplicatesAndCapsIDs(t *testing.T) {
	taskID := "normalize-retrieval-ids"
	tools.ClearRetrievalContext(taskID)
	t.Cleanup(func() { tools.ClearRetrievalContext(taskID) })
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.JITFetchMaxItems = 3 }))
	decision := &PlanDecision{Actions: []ActionCall{{Action: "rag_fetch", Parameters: map[string]any{
		"ids": []any{"rag-1", "rag-2", "rag-1", "rag-3", "rag-4"},
	}}}}
	changes := normalizeRetrievalActionParameters(&types.Task{ID: taskID}, decision)
	ids, _ := decision.Actions[0].Parameters["ids"].([]string)
	if len(ids) != 3 || ids[0] != "rag-1" || ids[1] != "rag-2" || ids[2] != "rag-3" {
		t.Fatalf("normalized ids=%v", ids)
	}
	if len(changes) != 1 || changes[0].OriginalCount != 5 || changes[0].PendingCount != 4 || changes[0].IncludedCount != 3 {
		t.Fatalf("normalization stats=%+v", changes)
	}
	if err := ValidateDecision(decision); err != nil {
		t.Fatalf("normalized decision must validate: %v", err)
	}
}

func TestNormalizeRetrievalActionParametersKeepsValidActionWhenAllIDsFetched(t *testing.T) {
	taskID := "normalize-already-fetched"
	tools.ClearRetrievalContext(taskID)
	t.Cleanup(func() { tools.ClearRetrievalContext(taskID) })
	oldTools := make(map[string]tools.Tool)
	for _, name := range []string{"rag_search", "rag_fetch", "memory_search", "memory_get"} {
		oldTools[name], _ = tools.Get(name)
	}
	tools.RegisterRetrievalTools(tools.RetrievalDependencies{SearchRAG: func(context.Context, string) ([]types.Memory, error) {
		return []types.Memory{{Goal: "result", KeyFindings: "evidence"}}, nil
	}})
	t.Cleanup(func() {
		for _, tool := range oldTools {
			if tool != nil {
				for {
					unwrapper, ok := tool.(interface{ Unwrap() tools.Tool })
					if !ok {
						break
					}
					tool = unwrapper.Unwrap()
				}
				tools.Register(tool)
			}
		}
	})
	ctx := tools.WithRetrievalExecutionContext(context.Background(), taskID, "default")
	search, _ := tools.Get("rag_search")
	result, err := search.Execute(ctx, "", map[string]any{"query": "query", "top_k": 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var payload struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.Observation), &payload); err != nil || len(payload.Results) != 1 {
		t.Fatalf("decode search result: payload=%+v err=%v", payload, err)
	}
	id := payload.Results[0].ID
	fetch, _ := tools.Get("rag_fetch")
	if _, err := fetch.Execute(ctx, "", map[string]any{"ids": []string{id}}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	decision := &PlanDecision{Actions: []ActionCall{{Action: "rag_fetch", Parameters: map[string]any{"ids": []string{id}}}}}
	changes := normalizeRetrievalActionParameters(&types.Task{ID: taskID}, decision)
	ids, _ := decision.Actions[0].Parameters["ids"].([]string)
	if len(ids) != 1 || ids[0] != id || len(changes) != 1 || !changes[0].AllAlreadyFetched || changes[0].PendingCount != 0 {
		t.Fatalf("ids=%v changes=%+v", ids, changes)
	}
	if err := ValidateDecision(decision); err != nil {
		t.Fatalf("all-fetched action must remain valid for orchestrator guard: %v", err)
	}
}
