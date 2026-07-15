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

func TestEnforceJITRetrievalDoesNotAffectReasoningTask(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.RAG.ContextMode = "jit" }))
	task := &types.Task{Goal: "计算 12 * 8"}
	decision := &PlanDecision{Stop: true, FinalAnswer: "96", Actions: []ActionCall{{Action: "none"}}}
	if enforceJITRetrieval(task, decision) {
		t.Fatal("reasoning-only answer should not require retrieval")
	}
}
