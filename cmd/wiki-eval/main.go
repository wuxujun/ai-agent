package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/wiki"
	"github.com/wuxujun/ai-agent/internal/wikieval"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wiki-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "evals/wiki_retrieval.jsonl", "JSONL evaluation dataset")
	directory := flags.String("directory", os.Getenv("AI_AGENT_WIKI_DIRECTORY"), "llm-wiki checkout root or wiki directory")
	space := flags.String("space", firstNonEmpty(os.Getenv("AI_AGENT_WIKI_DEFAULT_SPACE"), "local"), "Wiki space used to construct result URIs")
	topK := flags.Int("top-k", 5, "default search result limit")
	caseTimeout := flags.Duration("case-timeout", 10*time.Second, "timeout per case")
	format := flags.String("format", "text", "output format: text or json")
	thresholds := wikieval.Thresholds{}
	flags.Float64Var(&thresholds.MinRecallAtK, "min-recall", 0.80, "minimum mean Recall@K")
	flags.Float64Var(&thresholds.MinFirstHitRate, "min-first-hit-rate", 0.60, "minimum top-1 expected-page hit rate")
	flags.Float64Var(&thresholds.MinFetchSuccessRate, "min-fetch-success-rate", 0.95, "minimum expected-page fetch success rate")
	flags.Float64Var(&thresholds.MinKeywordCoverage, "min-keyword-coverage", 0.80, "minimum expected keyword coverage")
	flags.Float64Var(&thresholds.MinCitationCoverage, "min-citation-coverage", 1.0, "minimum wiki:// URI coverage")
	flags.Float64Var(&thresholds.MaxErrorRate, "max-error-rate", 0, "maximum case error rate")
	flags.Int64Var(&thresholds.MaxP95LatencyMS, "max-p95-ms", 500, "maximum local search+fetch P95 latency in milliseconds; 0 disables")
	flags.Float64Var(&thresholds.MinGraphPathRecall, "min-graph-path-recall", 0.80, "minimum expected graph-node recall")
	flags.Float64Var(&thresholds.MaxIrrelevantNodeRate, "max-irrelevant-node-rate", 0.75, "maximum unrelated graph-node rate")
	flags.Float64Var(&thresholds.MinSuggestionRecall, "min-suggestion-recall", 0.80, "minimum expected Wiki suggestion recall")
	flags.Float64Var(&thresholds.MaxSuggestionNoiseRate, "max-suggestion-noise-rate", 0.60, "maximum unexpected Wiki suggestion rate")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*directory) == "" || *topK <= 0 || *topK > 20 || *caseTimeout <= 0 || (*format != "text" && *format != "json") || !validThresholds(thresholds) {
		fmt.Fprintln(stderr, "directory, top-k 1..20, positive timeout, format text|json, and thresholds in [0,1] are required")
		return 2
	}
	file, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(stderr, "open input: %v\n", err)
		return 2
	}
	defer file.Close()
	cases, err := wikieval.LoadJSONL(file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	client, err := wiki.NewDirectory(*directory)
	if err == nil {
		err = client.Initialize(context.Background())
	}
	if err != nil {
		fmt.Fprintf(stderr, "initialize Wiki directory: %v\n", err)
		return 2
	}
	results := make([]wikieval.CaseResult, 0, len(cases))
	for _, item := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), *caseTimeout)
		result := wikieval.Evaluate(ctx, client, strings.Trim(*space, "/"), item, *topK)
		cancel()
		results = append(results, result)
	}
	summary := wikieval.Summarize(results, thresholds)
	if *format == "json" {
		encoder := json.NewEncoder(stdout)
		for _, result := range results {
			_ = encoder.Encode(result)
		}
		_ = encoder.Encode(summary)
	} else {
		for _, result := range results {
			fmt.Fprintf(stdout, "%s recall=%.3f first=%t rr=%.3f fetch=%t keywords=%.3f citations=%.3f graph_recall=%.3f irrelevant=%.3f nodes=%d edges=%d suggestion_recall=%.3f suggestion_noise=%.3f suggestions=%d latency=%dms error=%s\n",
				result.Name, result.RecallAtK, result.FirstHit, result.ReciprocalRank, result.FetchSucceeded,
				result.KeywordCoverage, result.CitationCoverage, result.GraphPathRecall, result.IrrelevantNodeRate,
				result.GraphNodes, result.GraphEdges, result.SuggestionRecall, result.SuggestionNoiseRate, result.SuggestionCount, result.LatencyMS, result.Error)
		}
		fmt.Fprintf(stdout, "summary cases=%d recall=%.3f first_hit=%.3f mrr=%.3f fetch=%.3f keywords=%.3f citations=%.3f graph_cases=%d graph_recall=%.3f irrelevant=%.3f suggestion_cases=%d suggestion_recall=%.3f suggestion_noise=%.3f errors=%.3f p95=%dms passed=%t failures=%s\n",
			summary.Cases, summary.RecallAtK, summary.FirstHitRate, summary.MeanReciprocalRank,
			summary.FetchSuccessRate, summary.KeywordCoverage, summary.CitationCoverage, summary.GraphCases,
			summary.GraphPathRecall, summary.IrrelevantNodeRate, summary.SuggestionCases, summary.SuggestionRecall, summary.SuggestionNoiseRate, summary.ErrorRate,
			summary.P95LatencyMS, summary.ThresholdsPassed, strings.Join(summary.FailedThresholds, "; "))
	}
	if !summary.ThresholdsPassed {
		return 1
	}
	return 0
}

func validThresholds(value wikieval.Thresholds) bool {
	for _, metric := range []float64{value.MinRecallAtK, value.MinFirstHitRate, value.MinFetchSuccessRate, value.MinKeywordCoverage, value.MinCitationCoverage, value.MaxErrorRate, value.MinGraphPathRecall, value.MaxIrrelevantNodeRate, value.MinSuggestionRecall, value.MaxSuggestionNoiseRate} {
		if metric < 0 || metric > 1 {
			return false
		}
	}
	return value.MaxP95LatencyMS >= 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
