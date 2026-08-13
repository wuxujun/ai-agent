package canarygate

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvaluatePromotesOnlyWhenEveryGatePasses(t *testing.T) {
	cfg := Config{
		Window:                 "24h",
		MinDAGSuccesses:        20,
		MinLegacyCalls:         20,
		MaxNonSuccessRateDelta: 0.05,
		MaxP95LatencyRatio:     1.20,
		MaxApprovalRateDelta:   0.05,
		MaxReplanRateDelta:     0.05,
		ManualReviewPassed:     true,
	}
	snapshot := Snapshot{
		DAGCalls:             21,
		DAGSuccesses:         20,
		LegacyCalls:          100,
		LegacySuccesses:      94,
		DAGP95LatencyMS:      110,
		LegacyP95LatencyMS:   100,
		DAGEventsObserved:    21,
		LegacyEventsObserved: 100,
	}
	report := Evaluate(cfg, snapshot)
	if report.Decision != "PROMOTE" || len(report.Reasons) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestEvaluateHoldsOnEveryReleaseRisk(t *testing.T) {
	cfg := Config{
		Window:                 "24h",
		MinDAGSuccesses:        20,
		MinLegacyCalls:         20,
		MaxNonSuccessRateDelta: 0.05,
		MaxP95LatencyRatio:     1.20,
		MaxApprovalRateDelta:   0.05,
		MaxReplanRateDelta:     0.05,
	}
	snapshot := Snapshot{
		DAGCalls:            12,
		DAGSuccesses:        10,
		LegacyCalls:         10,
		LegacySuccesses:     10,
		DAGFallbacks:        1,
		DAGP95LatencyMS:     130,
		LegacyP95LatencyMS:  100,
		DAGApprovalRequired: 2,
		DAGReplanned:        2,
	}
	report := Evaluate(cfg, snapshot)
	if report.Decision != "HOLD" {
		t.Fatalf("decision = %q", report.Decision)
	}
	joined := strings.Join(report.Reasons, "\n")
	for _, want := range []string{"DAG successes", "Legacy calls", "DAG fallbacks", "non-success rate", "P95 ratio", "approval-required rate", "replan rate", "manual review"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons missing %q: %s", want, joined)
		}
	}
}

func TestEvaluateHoldsWithoutComparableLatency(t *testing.T) {
	report := Evaluate(Config{
		Window:                 "24h",
		MinDAGSuccesses:        1,
		MinLegacyCalls:         1,
		MaxNonSuccessRateDelta: 0.05,
		MaxP95LatencyRatio:     1.20,
		MaxApprovalRateDelta:   0.05,
		MaxReplanRateDelta:     0.05,
		ManualReviewPassed:     true,
	}, Snapshot{DAGCalls: 1, DAGSuccesses: 1, LegacyCalls: 1, LegacySuccesses: 1, DAGP95LatencyMS: math.NaN()})
	if report.Decision != "HOLD" || !strings.Contains(strings.Join(report.Reasons, " "), "P95") {
		t.Fatalf("report = %+v", report)
	}
}

type queryMap map[string]float64

func (q queryMap) Query(_ context.Context, query string) (float64, error) {
	value, ok := q[query]
	if !ok {
		return 0, fmt.Errorf("unexpected query %q", query)
	}
	return value, nil
}

func TestCollectUsesWindowedRuntimeQueries(t *testing.T) {
	client := queryMap{
		`clamp_min(sum(agent_multiagent_runtime_calls_total{runtime="dag"}) - (sum(agent_multiagent_runtime_calls_total{runtime="dag"} offset 6h) or vector(0)), 0)`:                                                             21,
		`clamp_min(sum(agent_multiagent_runtime_calls_total{runtime="dag",outcome="success"}) - (sum(agent_multiagent_runtime_calls_total{runtime="dag",outcome="success"} offset 6h) or vector(0)), 0)`:                         20,
		`clamp_min(sum(agent_multiagent_runtime_calls_total{runtime="legacy"}) - (sum(agent_multiagent_runtime_calls_total{runtime="legacy"} offset 6h) or vector(0)), 0)`:                                                       100,
		`clamp_min(sum(agent_multiagent_runtime_calls_total{runtime="legacy",outcome="success"}) - (sum(agent_multiagent_runtime_calls_total{runtime="legacy",outcome="success"} offset 6h) or vector(0)), 0)`:                   98,
		`clamp_min(sum(agent_multiagent_runtime_fallbacks_total) - (sum(agent_multiagent_runtime_fallbacks_total offset 6h) or vector(0)), 0)`:                                                                                   0,
		`histogram_quantile(0.95, sum by (le) (increase(agent_multiagent_runtime_latency_ms_bucket{runtime="dag"}[6h])))`:                                                                                                        110,
		`histogram_quantile(0.95, sum by (le) (increase(agent_multiagent_runtime_latency_ms_bucket{runtime="legacy"}[6h])))`:                                                                                                     100,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="dag",event="approval_required"}) - (sum(agent_multiagent_runtime_events_total{runtime="dag",event="approval_required"} offset 6h) or vector(0)), 0)`:       1,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="legacy",event="approval_required"}) - (sum(agent_multiagent_runtime_events_total{runtime="legacy",event="approval_required"} offset 6h) or vector(0)), 0)`: 2,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="dag",event="replanned"}) - (sum(agent_multiagent_runtime_events_total{runtime="dag",event="replanned"} offset 6h) or vector(0)), 0)`:                       3,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="legacy",event="replanned"}) - (sum(agent_multiagent_runtime_events_total{runtime="legacy",event="replanned"} offset 6h) or vector(0)), 0)`:                 4,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="dag",event="observed"}) - (sum(agent_multiagent_runtime_events_total{runtime="dag",event="observed"} offset 6h) or vector(0)), 0)`:                         21,
		`clamp_min(sum(agent_multiagent_runtime_events_total{runtime="legacy",event="observed"}) - (sum(agent_multiagent_runtime_events_total{runtime="legacy",event="observed"} offset 6h) or vector(0)), 0)`:                   100,
	}
	snapshot, err := Collect(context.Background(), client, "6h")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DAGSuccesses != 20 || snapshot.LegacyCalls != 100 || snapshot.DAGP95LatencyMS != 110 || snapshot.DAGApprovalRequired != 1 || snapshot.LegacyReplanned != 4 || snapshot.DAGEventsObserved != 21 || snapshot.LegacyEventsObserved != 100 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestPrometheusClientParsesVectorAndEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("query") == "empty" {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[123,"7.5"]}]}}`)
	}))
	defer server.Close()
	client, err := NewPrometheusClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	value, err := client.Query(context.Background(), "value")
	if err != nil || value != 7.5 {
		t.Fatalf("value=%v err=%v", value, err)
	}
	value, err = client.Query(context.Background(), "empty")
	if err != nil || value != 0 {
		t.Fatalf("empty value=%v err=%v", value, err)
	}
}
