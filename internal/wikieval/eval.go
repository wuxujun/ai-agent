package wikieval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/wiki"
)

const defaultMaxLineBytes = 1 << 20

type Case struct {
	Name                   string   `json:"name"`
	Query                  string   `json:"query"`
	ExpectedURIs           []string `json:"expected_uris"`
	ExpectedKeywords       []string `json:"expected_keywords,omitempty"`
	ExpectedGraphURIs      []string `json:"expected_graph_uris,omitempty"`
	ExpectedSuggestionURIs []string `json:"expected_suggestion_uris,omitempty"`
	GraphDepth             int      `json:"graph_depth,omitempty"`
	GraphDirection         string   `json:"graph_direction,omitempty"`
	SuggestLimit           int      `json:"suggest_limit,omitempty"`
	TopK                   int      `json:"top_k,omitempty"`
}

type Thresholds struct {
	MinRecallAtK           float64
	MinFirstHitRate        float64
	MinFetchSuccessRate    float64
	MinKeywordCoverage     float64
	MinCitationCoverage    float64
	MaxErrorRate           float64
	MaxP95LatencyMS        int64
	MinGraphPathRecall     float64
	MaxIrrelevantNodeRate  float64
	MinSuggestionRecall    float64
	MaxSuggestionNoiseRate float64
}

type CaseResult struct {
	Name                string   `json:"name"`
	Query               string   `json:"query"`
	ReturnedURIs        []string `json:"returned_uris,omitempty"`
	RecallAtK           float64  `json:"recall_at_k"`
	FirstHit            bool     `json:"first_hit"`
	ReciprocalRank      float64  `json:"reciprocal_rank"`
	FetchSucceeded      bool     `json:"fetch_succeeded"`
	KeywordCoverage     float64  `json:"keyword_coverage"`
	CitationCoverage    float64  `json:"citation_coverage"`
	GraphPathRecall     float64  `json:"graph_path_recall,omitempty"`
	IrrelevantNodeRate  float64  `json:"irrelevant_node_rate,omitempty"`
	GraphNodes          int      `json:"graph_nodes,omitempty"`
	GraphEdges          int      `json:"graph_edges,omitempty"`
	SuggestionRecall    float64  `json:"suggestion_recall,omitempty"`
	SuggestionNoiseRate float64  `json:"suggestion_noise_rate,omitempty"`
	SuggestionCount     int      `json:"suggestion_count,omitempty"`
	SuggestionEvaluated bool     `json:"-"`
	LatencyMS           int64    `json:"latency_ms"`
	Error               string   `json:"error,omitempty"`
}

type Summary struct {
	Cases               int      `json:"cases"`
	RecallAtK           float64  `json:"recall_at_k"`
	FirstHitRate        float64  `json:"first_hit_rate"`
	MeanReciprocalRank  float64  `json:"mean_reciprocal_rank"`
	FetchSuccessRate    float64  `json:"fetch_success_rate"`
	KeywordCoverage     float64  `json:"keyword_coverage"`
	CitationCoverage    float64  `json:"citation_coverage"`
	ErrorRate           float64  `json:"error_rate"`
	P95LatencyMS        int64    `json:"p95_latency_ms"`
	GraphCases          int      `json:"graph_cases"`
	GraphPathRecall     float64  `json:"graph_path_recall,omitempty"`
	IrrelevantNodeRate  float64  `json:"irrelevant_node_rate,omitempty"`
	SuggestionCases     int      `json:"suggestion_cases"`
	SuggestionRecall    float64  `json:"suggestion_recall,omitempty"`
	SuggestionNoiseRate float64  `json:"suggestion_noise_rate,omitempty"`
	ThresholdsPassed    bool     `json:"thresholds_passed"`
	FailedThresholds    []string `json:"failed_thresholds,omitempty"`
}

type Reader interface {
	Search(context.Context, string, int, string) ([]wiki.Document, error)
	Read(context.Context, wiki.Document, string) (wiki.Document, error)
}

type GraphReader interface {
	Graph(context.Context, wiki.Document, string, int, string) (wiki.GraphResult, error)
}

type SuggestReader interface {
	Suggest(context.Context, wiki.Document, string, int) (wiki.SuggestResult, error)
}

func LoadJSONL(reader io.Reader) ([]Case, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), defaultMaxLineBytes)
	cases := make([]Case, 0)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var item Case
		if err := json.Unmarshal([]byte(text), &item); err != nil {
			return nil, fmt.Errorf("decode Wiki eval line %d: %w", line, err)
		}
		if err := validateCase(item); err != nil {
			return nil, fmt.Errorf("Wiki eval line %d: %w", line, err)
		}
		cases = append(cases, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Wiki eval dataset: %w", err)
	}
	if len(cases) == 0 {
		return nil, errors.New("Wiki eval dataset has no cases")
	}
	return cases, nil
}

func validateCase(item Case) error {
	if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Query) == "" {
		return errors.New("name and query are required")
	}
	if len(item.ExpectedURIs) == 0 {
		return errors.New("expected_uris must not be empty")
	}
	if item.TopK < 0 || item.TopK > 20 {
		return errors.New("top_k must be between 0 and 20")
	}
	for _, uri := range item.ExpectedURIs {
		if !strings.HasPrefix(strings.TrimSpace(uri), "wiki://") {
			return fmt.Errorf("expected URI %q must use wiki://", uri)
		}
	}
	if len(item.ExpectedGraphURIs) > 0 {
		if item.GraphDepth < 1 || item.GraphDepth > 2 {
			return errors.New("graph_depth must be 1 or 2 when expected_graph_uris is set")
		}
		for _, uri := range item.ExpectedGraphURIs {
			if !strings.HasPrefix(strings.TrimSpace(uri), "wiki://") {
				return fmt.Errorf("expected graph URI %q must use wiki://", uri)
			}
		}
	}
	if len(item.ExpectedSuggestionURIs) > 0 {
		if item.SuggestLimit < 0 || item.SuggestLimit > 10 {
			return errors.New("suggest_limit must be between 0 and 10")
		}
		for _, uri := range item.ExpectedSuggestionURIs {
			if !strings.HasPrefix(strings.TrimSpace(uri), "wiki://") {
				return fmt.Errorf("expected suggestion URI %q must use wiki://", uri)
			}
		}
	}
	return nil
}

func Evaluate(ctx context.Context, reader Reader, space string, item Case, defaultTopK int) CaseResult {
	result := CaseResult{Name: item.Name, Query: item.Query}
	if reader == nil {
		result.Error = "Wiki reader is nil"
		return result
	}
	topK := item.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	if topK <= 0 {
		topK = 5
	}
	started := time.Now()
	documents, err := reader.Search(ctx, item.Query, topK, space)
	if err != nil {
		result.Error = err.Error()
		result.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	expected := make(map[string]bool, len(item.ExpectedURIs))
	for _, uri := range item.ExpectedURIs {
		expected[strings.TrimSpace(uri)] = true
	}
	hits := 0
	citations := 0
	firstRank := 0
	var fetchDocument wiki.Document
	for index, document := range documents {
		result.ReturnedURIs = append(result.ReturnedURIs, document.URI)
		if strings.HasPrefix(document.URI, "wiki://") {
			citations++
		}
		if expected[document.URI] {
			hits++
			if firstRank == 0 {
				firstRank = index + 1
				fetchDocument = document
			}
		}
	}
	result.RecallAtK = float64(hits) / float64(len(expected))
	result.FirstHit = len(documents) > 0 && expected[documents[0].URI]
	if firstRank > 0 {
		result.ReciprocalRank = 1 / float64(firstRank)
	}
	if len(documents) > 0 {
		result.CitationCoverage = float64(citations) / float64(len(documents))
	}
	if firstRank > 0 {
		page, readErr := reader.Read(ctx, fetchDocument, space)
		if readErr != nil {
			result.Error = readErr.Error()
		} else {
			result.FetchSucceeded = strings.TrimSpace(page.Content) != ""
			result.KeywordCoverage = keywordCoverage(page.Content, item.ExpectedKeywords)
		}
		if len(item.ExpectedGraphURIs) > 0 && result.Error == "" {
			graphReader, ok := reader.(GraphReader)
			if !ok {
				result.Error = "Wiki reader does not support graph evaluation"
			} else {
				direction := item.GraphDirection
				if direction == "" {
					direction = "both"
				}
				graph, graphErr := graphReader.Graph(ctx, fetchDocument, space, item.GraphDepth, direction)
				if graphErr != nil {
					result.Error = graphErr.Error()
				} else {
					result.GraphNodes = len(graph.Nodes)
					result.GraphEdges = len(graph.Edges)
					result.GraphPathRecall, result.IrrelevantNodeRate = graphQuality(graph, item.ExpectedGraphURIs)
				}
			}
		}
		if len(item.ExpectedSuggestionURIs) > 0 && result.Error == "" {
			suggestReader, ok := reader.(SuggestReader)
			if !ok {
				result.Error = "Wiki reader does not support suggestion evaluation"
			} else {
				limit := item.SuggestLimit
				if limit == 0 {
					limit = 5
				}
				suggestions, suggestErr := suggestReader.Suggest(ctx, fetchDocument, space, limit)
				if suggestErr != nil {
					result.Error = suggestErr.Error()
				} else {
					result.SuggestionEvaluated = true
					result.SuggestionCount = len(suggestions.Suggestions)
					result.SuggestionRecall, result.SuggestionNoiseRate = suggestionQuality(suggestions, item.ExpectedSuggestionURIs)
				}
			}
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	return result
}

func Summarize(results []CaseResult, thresholds Thresholds) Summary {
	summary := Summary{Cases: len(results), ThresholdsPassed: true}
	if len(results) == 0 {
		summary.ThresholdsPassed = false
		summary.FailedThresholds = []string{"no evaluation results"}
		return summary
	}
	latencies := make([]int64, 0, len(results))
	for _, result := range results {
		summary.RecallAtK += result.RecallAtK
		summary.MeanReciprocalRank += result.ReciprocalRank
		summary.KeywordCoverage += result.KeywordCoverage
		summary.CitationCoverage += result.CitationCoverage
		if result.FirstHit {
			summary.FirstHitRate++
		}
		if result.FetchSucceeded {
			summary.FetchSuccessRate++
		}
		if result.Error != "" {
			summary.ErrorRate++
		}
		if result.GraphNodes > 0 {
			summary.GraphCases++
			summary.GraphPathRecall += result.GraphPathRecall
			summary.IrrelevantNodeRate += result.IrrelevantNodeRate
		}
		if result.SuggestionEvaluated {
			summary.SuggestionCases++
			summary.SuggestionRecall += result.SuggestionRecall
			summary.SuggestionNoiseRate += result.SuggestionNoiseRate
		}
		latencies = append(latencies, result.LatencyMS)
	}
	denominator := float64(len(results))
	summary.RecallAtK /= denominator
	summary.FirstHitRate /= denominator
	summary.MeanReciprocalRank /= denominator
	summary.FetchSuccessRate /= denominator
	summary.KeywordCoverage /= denominator
	summary.CitationCoverage /= denominator
	summary.ErrorRate /= denominator
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	summary.P95LatencyMS = latencies[(len(latencies)*95-1)/100]
	if summary.GraphCases > 0 {
		summary.GraphPathRecall /= float64(summary.GraphCases)
		summary.IrrelevantNodeRate /= float64(summary.GraphCases)
	}
	if summary.SuggestionCases > 0 {
		summary.SuggestionRecall /= float64(summary.SuggestionCases)
		summary.SuggestionNoiseRate /= float64(summary.SuggestionCases)
	}
	summary.FailedThresholds = failedThresholds(summary, thresholds)
	summary.ThresholdsPassed = len(summary.FailedThresholds) == 0
	return summary
}

func failedThresholds(summary Summary, thresholds Thresholds) []string {
	checks := []struct {
		failed bool
		text   string
	}{
		{summary.RecallAtK < thresholds.MinRecallAtK, fmt.Sprintf("recall_at_k %.3f < %.3f", summary.RecallAtK, thresholds.MinRecallAtK)},
		{summary.FirstHitRate < thresholds.MinFirstHitRate, fmt.Sprintf("first_hit_rate %.3f < %.3f", summary.FirstHitRate, thresholds.MinFirstHitRate)},
		{summary.FetchSuccessRate < thresholds.MinFetchSuccessRate, fmt.Sprintf("fetch_success_rate %.3f < %.3f", summary.FetchSuccessRate, thresholds.MinFetchSuccessRate)},
		{summary.KeywordCoverage < thresholds.MinKeywordCoverage, fmt.Sprintf("keyword_coverage %.3f < %.3f", summary.KeywordCoverage, thresholds.MinKeywordCoverage)},
		{summary.CitationCoverage < thresholds.MinCitationCoverage, fmt.Sprintf("citation_coverage %.3f < %.3f", summary.CitationCoverage, thresholds.MinCitationCoverage)},
		{summary.ErrorRate > thresholds.MaxErrorRate, fmt.Sprintf("error_rate %.3f > %.3f", summary.ErrorRate, thresholds.MaxErrorRate)},
		{thresholds.MaxP95LatencyMS > 0 && summary.P95LatencyMS > thresholds.MaxP95LatencyMS, fmt.Sprintf("p95_latency_ms %d > %d", summary.P95LatencyMS, thresholds.MaxP95LatencyMS)},
		{summary.GraphCases > 0 && summary.GraphPathRecall < thresholds.MinGraphPathRecall, fmt.Sprintf("graph_path_recall %.3f < %.3f", summary.GraphPathRecall, thresholds.MinGraphPathRecall)},
		{summary.GraphCases > 0 && summary.IrrelevantNodeRate > thresholds.MaxIrrelevantNodeRate, fmt.Sprintf("irrelevant_node_rate %.3f > %.3f", summary.IrrelevantNodeRate, thresholds.MaxIrrelevantNodeRate)},
		{summary.SuggestionCases > 0 && summary.SuggestionRecall < thresholds.MinSuggestionRecall, fmt.Sprintf("suggestion_recall %.3f < %.3f", summary.SuggestionRecall, thresholds.MinSuggestionRecall)},
		{summary.SuggestionCases > 0 && summary.SuggestionNoiseRate > thresholds.MaxSuggestionNoiseRate, fmt.Sprintf("suggestion_noise_rate %.3f > %.3f", summary.SuggestionNoiseRate, thresholds.MaxSuggestionNoiseRate)},
	}
	failures := make([]string, 0)
	for _, check := range checks {
		if check.failed {
			failures = append(failures, check.text)
		}
	}
	return failures
}

func graphQuality(graph wiki.GraphResult, expectedURIs []string) (float64, float64) {
	expected := make(map[string]bool, len(expectedURIs))
	for _, uri := range expectedURIs {
		expected[strings.TrimSpace(uri)] = true
	}
	hits := 0
	irrelevant := 0
	considered := 0
	for _, node := range graph.Nodes {
		if node.URI == graph.RootURI {
			continue
		}
		considered++
		if expected[node.URI] {
			hits++
		} else {
			irrelevant++
		}
	}
	rate := float64(0)
	if considered > 0 {
		rate = float64(irrelevant) / float64(considered)
	}
	return float64(hits) / float64(len(expected)), rate
}

func suggestionQuality(result wiki.SuggestResult, expectedURIs []string) (float64, float64) {
	expected := make(map[string]bool, len(expectedURIs))
	for _, uri := range expectedURIs {
		expected[strings.TrimSpace(uri)] = true
	}
	hits, noise := 0, 0
	seen := make(map[string]bool)
	for _, item := range result.Suggestions {
		if seen[item.URI] {
			continue
		}
		seen[item.URI] = true
		if expected[item.URI] {
			hits++
		} else {
			noise++
		}
	}
	noiseRate := float64(0)
	if len(seen) > 0 {
		noiseRate = float64(noise) / float64(len(seen))
	}
	return float64(hits) / float64(len(expected)), noiseRate
}

func keywordCoverage(content string, keywords []string) float64 {
	if len(keywords) == 0 {
		return 1
	}
	content = strings.ToLower(content)
	hits := 0
	for _, keyword := range keywords {
		if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" && strings.Contains(content, keyword) {
			hits++
		}
	}
	return float64(hits) / float64(len(keywords))
}
