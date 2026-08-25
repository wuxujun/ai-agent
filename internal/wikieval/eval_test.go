package wikieval

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/wiki"
)

type evalReader struct{}

func (evalReader) Search(context.Context, string, int, string) ([]wiki.Document, error) {
	return []wiki.Document{{URI: "wiki://local/concepts/other"}, {URI: "wiki://local/concepts/pbl", Slug: "concepts/pbl"}}, nil
}
func (evalReader) Read(context.Context, wiki.Document, string) (wiki.Document, error) {
	return wiki.Document{Content: "Students write an 800–1,000 word historical travel guide."}, nil
}
func (evalReader) Graph(context.Context, wiki.Document, string, int, string) (wiki.GraphResult, error) {
	return wiki.GraphResult{
		RootURI: "wiki://local/concepts/pbl",
		Nodes: []wiki.GraphNode{
			{URI: "wiki://local/concepts/pbl"},
			{URI: "wiki://local/entities/teacher"},
			{URI: "wiki://local/sources/course"},
			{URI: "wiki://local/concepts/unrelated"},
		},
		Edges: []wiki.GraphEdge{
			{From: "wiki://local/concepts/pbl", To: "wiki://local/entities/teacher"},
			{From: "wiki://local/concepts/pbl", To: "wiki://local/sources/course"},
		},
	}, nil
}
func (evalReader) Suggest(context.Context, wiki.Document, string, int) (wiki.SuggestResult, error) {
	return wiki.SuggestResult{Suggestions: []wiki.Suggestion{
		{Kind: "missing_link", URI: "wiki://local/entities/teacher"},
		{Kind: "related", URI: "wiki://local/concepts/unrelated"},
	}}, nil
}

func TestLoadEvaluateAndSummarize(t *testing.T) {
	cases, err := LoadJSONL(strings.NewReader(`{"name":"pbl","query":"PBL guide","expected_uris":["wiki://local/concepts/pbl"],"expected_keywords":["800–1,000","travel guide"],"top_k":3}` + "\n"))
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases=%+v err=%v", cases, err)
	}
	result := Evaluate(t.Context(), evalReader{}, "local", cases[0], 5)
	if result.RecallAtK != 1 || result.PrecisionAtK != 1.0/3.0 || result.NDCGAtK != 1/math.Log2(3) || result.FirstHit || !result.Top3Hit || result.ReciprocalRank != 0.5 || !result.FetchSucceeded || result.KeywordCoverage != 1 || result.CitationCoverage != 1 {
		t.Fatalf("result=%+v", result)
	}
	summary := Summarize([]CaseResult{result}, Thresholds{MinRecallAtK: 1, MinFetchSuccessRate: 1, MinKeywordCoverage: 1, MinCitationCoverage: 1, MaxErrorRate: 0, MaxP95LatencyMS: 1000})
	if !summary.ThresholdsPassed || summary.MeanReciprocalRank != 0.5 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestEvaluateNoAnswerCaseAndFalsePositiveGate(t *testing.T) {
	item := Case{Name: "no-answer", Query: "unknown", ExpectNoResults: true}
	result := Evaluate(t.Context(), evalReader{}, "local", item, 5)
	if !result.NoAnswerExpected || !result.NoAnswerFalsePositive || result.FetchSucceeded {
		t.Fatalf("result=%+v", result)
	}
	summary := Summarize([]CaseResult{result}, Thresholds{MaxNoAnswerFalsePositiveRate: 0})
	if summary.ThresholdsPassed || summary.NoAnswerCases != 1 || summary.NoAnswerFalsePositiveRate != 1 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestEvaluateGraphQualityAndThresholds(t *testing.T) {
	cases, err := LoadJSONL(strings.NewReader(`{"name":"pbl-graph","query":"PBL guide","expected_uris":["wiki://local/concepts/pbl"],"expected_graph_uris":["wiki://local/entities/teacher","wiki://local/sources/course"],"graph_depth":2,"graph_direction":"both"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := Evaluate(t.Context(), evalReader{}, "local", cases[0], 5)
	if result.Error != "" || result.GraphPathRecall != 1 || result.IrrelevantNodeRate != 1.0/3.0 || result.GraphNodes != 4 || result.GraphEdges != 2 {
		t.Fatalf("result=%+v", result)
	}
	summary := Summarize([]CaseResult{result}, Thresholds{MinGraphPathRecall: 1, MaxIrrelevantNodeRate: 0.34})
	if !summary.ThresholdsPassed || summary.GraphCases != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	failed := Summarize([]CaseResult{result}, Thresholds{MinGraphPathRecall: 1, MaxIrrelevantNodeRate: 0.3})
	if failed.ThresholdsPassed || len(failed.FailedThresholds) != 1 {
		t.Fatalf("failed summary=%+v", failed)
	}
}

func TestEvaluateSuggestionQualityAndThresholds(t *testing.T) {
	cases, err := LoadJSONL(strings.NewReader(`{"name":"pbl-suggest","query":"PBL guide","expected_uris":["wiki://local/concepts/pbl"],"expected_suggestion_uris":["wiki://local/entities/teacher"],"suggest_limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	result := Evaluate(t.Context(), evalReader{}, "local", cases[0], 5)
	if result.Error != "" || !result.SuggestionEvaluated || result.SuggestionRecall != 1 || result.SuggestionNoiseRate != 0.5 || result.SuggestionCount != 2 {
		t.Fatalf("result=%+v", result)
	}
	summary := Summarize([]CaseResult{result}, Thresholds{MinSuggestionRecall: 1, MaxSuggestionNoiseRate: 0.5})
	if !summary.ThresholdsPassed || summary.SuggestionCases != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	failed := Summarize([]CaseResult{result}, Thresholds{MinSuggestionRecall: 1, MaxSuggestionNoiseRate: 0.4})
	if failed.ThresholdsPassed || len(failed.FailedThresholds) != 1 {
		t.Fatalf("failed summary=%+v", failed)
	}
}

func TestLoadRejectsInvalidCaseAndThresholdsFail(t *testing.T) {
	if _, err := LoadJSONL(strings.NewReader(`{"name":"bad","query":"q","expected_uris":["https://example.test"]}`)); err == nil {
		t.Fatal("invalid URI accepted")
	}
	if _, err := LoadJSONL(strings.NewReader(`{"name":"bad","query":"q","expected_uris":["wiki://local/x"],"expect_no_results":true}`)); err == nil {
		t.Fatal("ambiguous no-answer case accepted")
	}
	if _, err := LoadJSONL(strings.NewReader(`{"name":"bad-graph","query":"q","expected_uris":["wiki://local/x"],"expected_graph_uris":["wiki://local/y"],"graph_depth":3}`)); err == nil {
		t.Fatal("invalid graph depth accepted")
	}
	summary := Summarize([]CaseResult{{RecallAtK: 0.5, Error: "failed", LatencyMS: 20}}, Thresholds{MinRecallAtK: 1, MaxErrorRate: 0, MaxP95LatencyMS: 10})
	if summary.ThresholdsPassed || len(summary.FailedThresholds) != 3 {
		t.Fatalf("summary=%+v", summary)
	}
}
