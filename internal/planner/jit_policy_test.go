package planner

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestEnforceJITRetrievalOverridesUnsupportedFactualAnswer(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.SearchURL = "https://rag.test/mcp"
	}))
	task := &types.Task{Goal: "汇总数学科学术顾问信息"}
	decision := &PlanDecision{Stop: true, FinalAnswer: "invented", Actions: []ActionCall{{Action: "none"}}}
	if !enforceJITRetrieval(task, decision) {
		t.Fatal("expected factual answer without evidence to be overridden")
	}
	if decision.Stop || decision.FinalAnswer != "" || len(decision.Actions) != 1 || decision.Actions[0].Action != "rag_search" {
		t.Fatalf("unexpected rewritten decision: %#v", decision)
	}
}

func TestEnforceJITRetrievalOverridesWorkspaceToolForExternalFact(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.SearchURL = "https://rag.test/mcp"
	}))
	task := &types.Task{Goal: "数学科学术顾问有哪个人？"}
	decision := &PlanDecision{Actions: []ActionCall{{Action: "find_files", Parameters: map[string]any{"pattern": "*.*"}}}}
	if !enforceJITRetrieval(task, decision) {
		t.Fatal("expected workspace action for external fact to be overridden")
	}
	if decision.Stop || len(decision.Actions) != 1 || decision.Actions[0].Action != "rag_search" {
		t.Fatalf("unexpected rewritten decision: %#v", decision)
	}
}

func TestEnforceJITRetrievalOverridesNextActionWithCandidateFetch(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.JITFetchMaxItems = 1
	}))
	task := &types.Task{Goal: "查询教师信息", Trace: []types.StepTrace{{
		Action:      "rag_search",
		Observation: `{"count":1,"results":[{"id":"rag-a"}]}`,
	}}}
	decision := &PlanDecision{Actions: []ActionCall{{Action: "search_text", Parameters: map[string]any{"query": "教师"}}}}
	if !enforceJITRetrieval(task, decision) || decision.Actions[0].Action != "rag_fetch" {
		t.Fatalf("candidate details were not forced before another action: %#v", decision)
	}
}

func TestEnforceJITRetrievalAllowsAnswerAfterEvidence(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "jit" }))
	task := &types.Task{Goal: "查询教师信息", Trace: []types.StepTrace{{Action: "rag_fetch", Observation: "fetched 1 rag item(s)", Evidence: []types.Evidence{{Lines: []string{"fact"}}}}}}
	decision := &PlanDecision{Stop: true, FinalAnswer: "supported", Actions: []ActionCall{{Action: "none"}}}
	if enforceJITRetrieval(task, decision) {
		t.Fatal("supported answer should not be overridden")
	}
}

func TestEnforceJITRetrievalFetchesSearchCandidatesBeforeAnswer(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.JITFetchMaxItems = 2
	}))
	task := &types.Task{
		Goal: "查询教师信息",
		Trace: []types.StepTrace{{
			Action:      "rag_search",
			Observation: `{"count":3,"results":[{"id":"rag-a"},{"id":"rag-b"},{"id":"rag-c"}]}`,
		}},
	}
	decision := &PlanDecision{Stop: true, FinalAnswer: "candidate snippet answer", Actions: []ActionCall{{Action: "none"}}}
	if !enforceJITRetrieval(task, decision) {
		t.Fatal("expected search-only answer to be overridden")
	}
	if decision.Stop || len(decision.Actions) != 1 || decision.Actions[0].Action != "rag_fetch" {
		t.Fatalf("decision=%+v, want rag_fetch", decision)
	}
	ids, _ := decision.Actions[0].Parameters["ids"].([]string)
	if len(ids) != 2 || ids[0] != "rag-a" || ids[1] != "rag-b" {
		t.Fatalf("ids=%v, want first two candidates", ids)
	}
}

func TestEnforceJITRetrievalStopsTruthfullyAfterEmptySearch(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
	}))
	task := &types.Task{Goal: "查询教师信息", Trace: []types.StepTrace{{Action: "memory_search", Observation: `{"count":0,"results":[]}`}}}
	decision := &PlanDecision{Stop: true, FinalAnswer: "invented", Actions: []ActionCall{{Action: "none"}}}
	if !enforceJITRetrieval(task, decision) {
		t.Fatal("expected empty-search answer to be overridden")
	}
	if !decision.Stop || decision.FinalAnswer == "invented" || decision.Actions[0].Action != "none" {
		t.Fatalf("unexpected decision after empty retrieval: %+v", decision)
	}
}

func TestEnforceJITRetrievalStopsAfterEmptyDetailFetch(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "jit" }))
	task := &types.Task{Goal: "教师陈园青简介", Trace: []types.StepTrace{
		{Action: "rag_search", Observation: `{"count":1,"results":[{"id":"rag-a"}]}`},
		{Action: "rag_fetch", Observation: "fetched 0 rag item(s)"},
	}}
	decision := &PlanDecision{Stop: true, FinalAnswer: "invented", Actions: []ActionCall{{Action: "none"}}}
	if !enforceJITRetrieval(task, decision) || !decision.Stop || decision.FinalAnswer == "invented" {
		t.Fatalf("empty detail fetch was not handled truthfully: %+v", decision)
	}
}

func TestRequiresFactualEvidenceUsesResearchIntent(t *testing.T) {
	task := &types.Task{Goal: "Provide a briefing", Trace: []types.StepTrace{{Action: "intent_route", Query: "research"}}}
	if !RequiresFactualEvidence(task) {
		t.Fatal("research intent should require supporting evidence")
	}
}

func TestRequiresFactualEvidenceLetsCodingIntentOverrideLexicalMarker(t *testing.T) {
	task := &types.Task{Goal: "修改教师信息页面代码", Trace: []types.StepTrace{{Action: "intent_route", Query: "coding"}}}
	if RequiresFactualEvidence(task) {
		t.Fatal("coding intent should not be forced into external fact retrieval")
	}
}

func TestRequiresFactualEvidenceExcludesExplicitWorkspaceLookup(t *testing.T) {
	task := &types.Task{Goal: "在项目中查找教师信息页面"}
	if RequiresFactualEvidence(task) {
		t.Fatal("explicit workspace lookup should use workspace tools")
	}
}

func TestWorkspaceSearchDoesNotSupportExternalFact(t *testing.T) {
	traces := []types.StepTrace{{Action: "search_text", Observation: "found 8 evidence items", Evidence: []types.Evidence{{Path: "a.md", Lines: []string{"顾问"}}}}}
	if HasSupportingEvidence(traces) {
		t.Fatal("workspace search must not count as external factual evidence")
	}
}

func TestEnforceJITRetrievalDoesNotAffectReasoningTask(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "jit" }))
	task := &types.Task{Goal: "计算 12 * 8"}
	decision := &PlanDecision{Stop: true, FinalAnswer: "96", Actions: []ActionCall{{Action: "none"}}}
	if enforceJITRetrieval(task, decision) {
		t.Fatal("reasoning-only answer should not require retrieval")
	}
}
