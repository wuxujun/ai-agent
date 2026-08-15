package planner

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
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
	if task == nil || !strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") || !RequiresFactualEvidence(task) {
		return nil, false
	}
	if state := tools.RetrievalStateForTask(task.ID); len(state.Searches) > 0 {
		latest := state.Searches[len(state.Searches)-1]
		if len(latest.PendingIDs) > 0 {
			fetchAction := "rag_fetch"
			if latest.Kind == "memory" {
				fetchAction = "memory_get"
			}
			limit := config.Get().RAG.JITFetchMaxItems
			if limit <= 0 {
				limit = 3
			}
			ids := append([]string(nil), latest.PendingIDs...)
			if len(ids) > limit {
				ids = ids[:limit]
			}
			return &PlanDecision{
				ThoughtSummary: "Fetch only retrieval candidates not fetched yet",
				Actions:        []ActionCall{{Action: fetchAction, Parameters: map[string]any{"ids": ids}}},
			}, true
		}
		if len(latest.CandidateIDs) == 0 && !HasSupportingEvidence(task.Trace) {
			return &PlanDecision{
				Stop: true, FinalAnswer: "未检索到足够证据，暂时无法可靠回答该事实性问题。",
				ThoughtSummary: "Stop because retrieval returned no usable evidence",
				Actions:        []ActionCall{{Action: "none", Parameters: map[string]any{}}},
			}, true
		}
	}
	if search, found := latestPendingRetrievalSearch(task.Trace); found {
		if len(search.IDs) == 0 {
			if HasSupportingEvidence(task.Trace) {
				return nil, false
			}
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
		} else if search.Action == "wiki_search" {
			fetchAction = "wiki_fetch"
		}
		return &PlanDecision{
			ThoughtSummary: "Fetch selected retrieval evidence before answering",
			Actions:        []ActionCall{{Action: fetchAction, Parameters: map[string]any{"ids": search.IDs}}},
		}, true
	}
	if HasSupportingEvidence(task.Trace) {
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
	action, ok := PreferredJITSearchAction(task)
	if !ok {
		return nil, false
	}
	return &PlanDecision{
		ThoughtSummary: "Retrieve evidence before answering the factual lookup request",
		Actions:        []ActionCall{{Action: action, Parameters: map[string]any{"query": task.Goal, "top_k": 5}}},
	}, true
}

func latestPendingRetrievalSearch(traces []types.StepTrace) (retrievalSearchState, bool) {
	for i := len(traces) - 1; i >= 0; i-- {
		switch traces[i].Action {
		case "wiki_fetch", "rag_fetch", "memory_get":
			return retrievalSearchState{}, false
		case "wiki_search", "rag_search", "memory_search":
			return retrievalSearchFromTrace(traces[i]), true
		}
	}
	return retrievalSearchState{}, false
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
	if goalExplicitlyTargetsMemory(task.Goal) {
		return "memory_search", true
	}
	if strings.TrimSpace(config.Get().Wiki.URL) != "" {
		if _, ready := tools.Get("wiki_search"); ready {
			return "wiki_search", true
		}
	}
	if strings.TrimSpace(config.Get().RAG.SearchURL) != "" {
		return "rag_search", true
	}
	return "memory_search", true
}

func goalExplicitlyTargetsMemory(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	markers := []string{
		"会话记忆", "历史记忆", "任务记忆", "对话记忆", "记忆中", "记忆里",
		"session memory", "conversation memory", "task memory", "memory history", "from memory", "in memory",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// RequiresFactualEvidence identifies tasks whose answer is likely to depend on
// externally verifiable facts. Intent routing is preferred when available;
// lexical markers provide a deterministic fallback when that optional scene is
// disabled.
func RequiresFactualEvidence(task *types.Task) bool {
	if task == nil {
		return false
	}
	if GoalExplicitlyTargetsWorkspace(task.Goal) {
		return false
	}
	if goalExplicitlyTargetsMemory(task.Goal) {
		return true
	}
	// An explicit external-source request is stronger than the optional intent
	// route. Software-team goals can otherwise be classified as "coding" merely
	// because they mention a runtime, even when the requested evidence must come
	// from the configured external knowledge source.
	if goalExplicitlyTargetsExternalKnowledge(task.Goal) {
		return true
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

func goalExplicitlyTargetsExternalKnowledge(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	markers := []string{
		"外部知识源", "外部数据源", "权威知识源", "权威外部来源",
		"external knowledge source", "external data source", "authoritative external source",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

var workspaceRepoWordPattern = regexp.MustCompile(`(?:^|[^a-z0-9_])repo(?:$|[^a-z0-9_])`)

func GoalExplicitlyTargetsWorkspace(goal string) bool {
	normalized := strings.ToLower(strings.TrimSpace(goal))
	markers := []string{
		"工作区", "项目中", "项目内", "仓库中", "仓库内", "本地文件", "源代码", "代码库",
		"workspace", "repository", "source code", "local file", "project file",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return workspaceRepoWordPattern.MatchString(normalized)
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
		case "wiki_fetch", "rag_fetch", "memory_get":
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
		case "wiki_fetch", "rag_fetch", "memory_get":
			return traces[i].Error != "" || len(traces[i].Evidence) == 0
		case "wiki_search", "rag_search", "memory_search":
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
		if trace.Action != "wiki_search" && trace.Action != "rag_search" && trace.Action != "memory_search" {
			continue
		}
		return retrievalSearchFromTrace(trace), true
	}
	return retrievalSearchState{}, false
}

func retrievalSearchFromTrace(trace types.StepTrace) retrievalSearchState {
	state := retrievalSearchState{Action: trace.Action}
	if trace.Error != "" {
		return state
	}
	var payload struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(trace.Observation), &payload) == nil {
		limit := config.Get().RAG.JITFetchMaxItems
		if trace.Action == "wiki_search" {
			limit = config.Get().Wiki.FetchMaxItems
		}
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
	return state
}
