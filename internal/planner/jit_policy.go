package planner

import (
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

// enforceJITRetrieval keeps factual lookup tasks on the configured retrieval
// path until real detail evidence has been fetched. It intentionally does not
// affect reasoning-only or workspace tasks.
func enforceJITRetrieval(task *types.Task, decision *PlanDecision) bool {
	if decision == nil {
		return false
	}
	routed, ok := NextJITRetrievalDecision(task)
	if !ok {
		return false
	}
	if samePlanDecision(decision, routed) {
		return false
	}
	*decision = *routed
	return true
}

// NextJITRetrievalDecision returns a deterministic retrieval decision when a
// factual lookup still needs evidence. Callers can use it before invoking an
// LLM, avoiding a planning round-trip whose only valid outcome is retrieval.
func NextJITRetrievalDecision(task *types.Task) (*PlanDecision, bool) {
	if task == nil || !strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") || !RequiresFactualEvidence(task) || HasSupportingEvidence(task.Trace) {
		return nil, false
	}
	if latestRetrievalDetailFailed(task.Trace) {
		return &PlanDecision{
			Stop:           true,
			FinalAnswer:    "检索详情未返回可用证据，暂时无法可靠回答该事实性问题。",
			ThoughtSummary: "Stop because retrieval details contained no usable evidence",
			Actions:        []ActionCall{{Action: "none", Parameters: map[string]any{}}},
		}, true
	}
	if search, found := latestRetrievalSearch(task.Trace); found {
		if len(search.IDs) == 0 {
			return &PlanDecision{
				Stop:           true,
				FinalAnswer:    "未检索到足够证据，暂时无法可靠回答该事实性问题。",
				ThoughtSummary: "Stop because retrieval returned no usable evidence",
				Actions:        []ActionCall{{Action: "none", Parameters: map[string]any{}}},
			}, true
		}
		fetchAction := "rag_fetch"
		if search.Action == "memory_search" {
			fetchAction = "memory_get"
		}
		return &PlanDecision{
			ThoughtSummary: "Fetch selected retrieval evidence before answering",
			Actions:        []ActionCall{{Action: fetchAction, Parameters: map[string]any{"ids": search.IDs}}},
		}, true
	}
	action, ok := PreferredJITSearchAction(task)
	if !ok {
		return nil, false
	}
	return &PlanDecision{
		ThoughtSummary: "Retrieve evidence before answering the factual lookup request",
		Actions:        []ActionCall{{Action: action, Parameters: map[string]any{"query": task.Goal, "top_k": 5}}},
	}, true
}

func samePlanDecision(left, right *PlanDecision) bool {
	if left == nil || right == nil || left.Stop != right.Stop || left.FinalAnswer != right.FinalAnswer || len(left.Actions) != len(right.Actions) {
		return false
	}
	for i := range left.Actions {
		if left.Actions[i].Action != right.Actions[i].Action {
			return false
		}
	}
	return true
}

// PreferredJITSearchAction returns the deterministic first retrieval action for
// an external factual lookup. RAG is preferred when configured; local memory is
// the compatibility fallback when no current external endpoint is available.
func PreferredJITSearchAction(task *types.Task) (string, bool) {
	if task == nil || !strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") || !RequiresFactualEvidence(task) {
		return "", false
	}
	if strings.TrimSpace(config.Get().RAG.SearchURL) != "" {
		return "rag_search", true
	}
	return "memory_search", true
}

// RequiresFactualEvidence identifies tasks whose answer is likely to depend on
// externally verifiable facts. Intent routing is preferred when available;
// lexical markers provide a deterministic fallback when that optional scene is
// disabled.
func RequiresFactualEvidence(task *types.Task) bool {
	if task == nil {
		return false
	}
	if goalExplicitlyTargetsWorkspace(task.Goal) {
		return false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		if task.Trace[i].Action != "intent_route" {
			continue
		}
		switch task.Trace[i].Query {
		case "research":
			return true
		case "coding", "writing", "data_analysis", "automation":
			return false
		}
		break
	}
	return goalLikelyNeedsRetrieval(task.Goal)
}

func goalExplicitlyTargetsWorkspace(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	markers := []string{
		"工作区", "项目中", "项目内", "仓库中", "仓库内", "本地文件", "源代码", "代码库",
		"workspace", "repository", "repo", "source code", "local file", "project file",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func goalLikelyNeedsRetrieval(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	if normalized == "" {
		return false
	}
	markers := []string{
		"查", "查询", "搜索", "检索", "信息", "资料", "汇总", "最近", "当前", "最新", "是谁", "有哪些",
		"简介", "名单", "老师", "教师", "顾问", "天气", "台风", "新闻", "价格", "何时", "哪里",
		"look up", "lookup", "search", "find information", "latest", "current", "who is", "summarize information",
		"profile", "teacher", "advisor", "weather", "news", "price", "when is", "where is",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func HasSupportingEvidence(traces []types.StepTrace) bool {
	for _, trace := range traces {
		if trace.Error != "" {
			continue
		}
		switch trace.Action {
		case "rag_fetch", "memory_get":
			if len(trace.Evidence) > 0 {
				return true
			}
		case "web_search", "http_fetch", "web_browser":
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

func latestRetrievalDetailFailed(traces []types.StepTrace) bool {
	for i := len(traces) - 1; i >= 0; i-- {
		switch traces[i].Action {
		case "rag_fetch", "memory_get":
			return traces[i].Error != "" || len(traces[i].Evidence) == 0
		case "rag_search", "memory_search":
			return false
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
