package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/wuxujun/ai-agent/internal/canarygate"
)

var prometheusDuration = regexp.MustCompile(`^[1-9][0-9]*(ms|s|m|h|d|w|y)$`)

func main() {
	os.Exit(run())
}

func run() int {
	var cfg canarygate.Config
	var prometheusURL string
	var jsonOutput bool
	flag.StringVar(&prometheusURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus base URL")
	flag.StringVar(&cfg.Window, "window", "24h", "Prometheus observation window")
	flag.Float64Var(&cfg.MinDAGSuccesses, "min-dag-successes", 20, "minimum successful DAG tasks")
	flag.Float64Var(&cfg.MinLegacyCalls, "min-legacy-calls", 20, "minimum Legacy baseline calls")
	flag.Float64Var(&cfg.MaxNonSuccessRateDelta, "max-non-success-delta", 0.05, "maximum DAG minus Legacy non-success rate")
	flag.Float64Var(&cfg.MaxP95LatencyRatio, "max-p95-ratio", 1.20, "maximum DAG/Legacy P95 latency ratio")
	flag.Float64Var(&cfg.MaxApprovalRateDelta, "max-approval-delta", 0.05, "maximum DAG minus Legacy approval-required rate")
	flag.Float64Var(&cfg.MaxReplanRateDelta, "max-replan-delta", 0.05, "maximum DAG minus Legacy replan rate")
	flag.BoolVar(&cfg.ManualReviewPassed, "manual-review-passed", false, "confirm approval and Replan behavior was reviewed")
	flag.BoolVar(&jsonOutput, "json", false, "print the report as JSON")
	flag.Parse()

	if !prometheusDuration.MatchString(cfg.Window) {
		fmt.Fprintf(os.Stderr, "invalid --window %q; use a Prometheus duration such as 30m or 24h\n", cfg.Window)
		return 2
	}
	if cfg.MinDAGSuccesses < 1 || cfg.MinLegacyCalls < 1 || cfg.MaxNonSuccessRateDelta < 0 || cfg.MaxP95LatencyRatio <= 0 || cfg.MaxApprovalRateDelta < 0 || cfg.MaxReplanRateDelta < 0 {
		fmt.Fprintln(os.Stderr, "invalid gate thresholds")
		return 2
	}
	client, err := canarygate.NewPrometheusClient(prometheusURL, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshot, err := canarygate.Collect(ctx, client, cfg.Window)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	report := canarygate.Evaluate(cfg, snapshot)
	if jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println(string(encoded))
	} else {
		printReport(report)
	}
	if report.Decision == "PROMOTE" {
		return 0
	}
	return 1
}

func printReport(report canarygate.Report) {
	fmt.Printf("decision=%s window=%s\n", report.Decision, report.Window)
	fmt.Printf("DAG: calls=%.0f successes=%.0f non_success_rate=%.2f%% p95_ms=%.2f fallbacks=%.0f\n",
		report.Snapshot.DAGCalls, report.Snapshot.DAGSuccesses, report.DAGNonSuccessRate*100,
		report.Snapshot.DAGP95LatencyMS, report.Snapshot.DAGFallbacks)
	fmt.Printf("Legacy: calls=%.0f successes=%.0f non_success_rate=%.2f%% p95_ms=%.2f\n",
		report.Snapshot.LegacyCalls, report.Snapshot.LegacySuccesses, report.LegacyNonSuccessRate*100,
		report.Snapshot.LegacyP95LatencyMS)
	fmt.Printf("p95_ratio=%.4f manual_review_passed=%t\n", report.P95LatencyRatio, report.ManualReviewPassed)
	fmt.Printf("approval_required_rate: DAG=%.2f%% Legacy=%.2f%%; replan_rate: DAG=%.2f%% Legacy=%.2f%%\n",
		report.DAGApprovalRate*100, report.LegacyApprovalRate*100, report.DAGReplanRate*100, report.LegacyReplanRate*100)
	fmt.Printf("runtime_event_coverage: DAG=%.0f/%.0f Legacy=%.0f/%.0f\n",
		report.Snapshot.DAGEventsObserved, report.Snapshot.DAGCalls, report.Snapshot.LegacyEventsObserved, report.Snapshot.LegacyCalls)
	for _, reason := range report.Reasons {
		fmt.Printf("- %s\n", reason)
	}
}
