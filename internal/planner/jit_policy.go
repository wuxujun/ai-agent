package planner

import (
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

// enforceJITRetrieval prevents an LLM from immediately fabricating an answer
// for an obvious lookup task before any retrieval/tool observation exists.
// It is intentionally narrow: reasoning-only and workspace tasks are not
// rewritten, and once a successful retrieval exists the planner owns the flow.
func enforceJITRetrieval(task *types.Task, decision *PlanDecision) bool {
	if task == nil || decision == nil || !decision.Stop || !strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") {
		return false
	}
	if !goalLikelyNeedsRetrieval(task.Goal) || hasSuccessfulRetrieval(task.Trace) {
		return false
	}
	if search, found := latestRetrievalSearch(task.Trace); found {
		if len(search.IDs) == 0 {
			decision.Stop = true
			decision.FinalAnswer = "未检索到足够证据，暂时无法可靠回答该事实性问题。"
			decision.ThoughtSummary = "Stop because retrieval returned no usable evidence"
			decision.Actions = []ActionCall{{Action: "none", Parameters: map[string]any{}}}
			return true
		}
		fetchAction := "rag_fetch"
		if search.Action == "memory_search" {
			fetchAction = "memory_get"
		}
		decision.Stop = false
		decision.FinalAnswer = ""
		decision.ThoughtSummary = "Fetch selected retrieval evidence before answering"
		decision.Actions = []ActionCall{{Action: fetchAction, Parameters: map[string]any{"ids": search.IDs}}}
		return true
	}
	action := "memory_search"
	if strings.TrimSpace(config.Get().RAG.SearchURL) != "" {
		action = "rag_search"
	}
	decision.Stop = false
	decision.FinalAnswer = ""
	decision.ThoughtSummary = "Retrieve evidence before answering the factual lookup request"
	decision.Actions = []ActionCall{{Action: action, Parameters: map[string]any{"query": task.Goal, "top_k": 5}}}
	return true
}

func goalLikelyNeedsRetrieval(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	if normalized == "" {
		return false
	}
	markers := []string{
		"查", "查询", "搜索", "检索", "信息", "资料", "汇总", "最近", "当前", "最新", "是谁", "有哪些",
		"look up", "lookup", "search", "find information", "latest", "current", "who is", "summarize information",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func hasSuccessfulRetrieval(traces []types.StepTrace) bool {
	for _, trace := range traces {
		if trace.Error != "" {
			continue
		}
		switch trace.Action {
		case "rag_fetch", "memory_get", "web_search", "http_fetch", "web_browser", "search_text", "read_file", "sql_query":
			observation := strings.ToLower(strings.TrimSpace(trace.Observation))
			if observation != "" && !strings.Contains(observation, `"count":0`) && !strings.Contains(observation, "found 0 evidence") && !strings.HasPrefix(observation, "error:") {
				return true
			}
			if len(trace.Evidence) > 0 {
				return true
			}
		}
	}
	return false
}

type retrievalSearchState struct {
	Action string
	IDs    []string
}

func latestRetrievalSearch(traces []types.StepTrace) (retrievalSearchState, bool) {
	for i := len(traces) - 1; i >= 0; i-- {
		trace := traces[i]
		if trace.Action != "rag_search" && trace.Action != "memory_search" {
			continue
		}
		state := retrievalSearchState{Action: trace.Action}
		if trace.Error != "" {
			return state, true
		}
		var payload struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if json.Unmarshal([]byte(trace.Observation), &payload) == nil {
			limit := config.Get().RAG.JITFetchMaxItems
			if limit <= 0 {
				limit = 3
			}
			for _, result := range payload.Results {
				if strings.TrimSpace(result.ID) != "" {
					state.IDs = append(state.IDs, result.ID)
				}
				if len(state.IDs) >= limit {
					break
				}
			}
		}
		return state, true
	}
	return retrievalSearchState{}, false
}
