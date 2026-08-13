package canarygate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Config defines the promotion requirements for a DAG canary observation window.
type Config struct {
	Window                 string
	MinDAGSuccesses        float64
	MinLegacyCalls         float64
	MaxNonSuccessRateDelta float64
	MaxP95LatencyRatio     float64
	MaxApprovalRateDelta   float64
	MaxReplanRateDelta     float64
	ManualReviewPassed     bool
}

// Snapshot contains the Prometheus values used by the gate.
type Snapshot struct {
	DAGCalls               float64 `json:"dag_calls"`
	DAGSuccesses           float64 `json:"dag_successes"`
	LegacyCalls            float64 `json:"legacy_calls"`
	LegacySuccesses        float64 `json:"legacy_successes"`
	DAGFallbacks           float64 `json:"dag_fallbacks"`
	DAGP95LatencyMS        float64 `json:"dag_p95_latency_ms"`
	LegacyP95LatencyMS     float64 `json:"legacy_p95_latency_ms"`
	DAGApprovalRequired    float64 `json:"dag_approval_required"`
	LegacyApprovalRequired float64 `json:"legacy_approval_required"`
	DAGReplanned           float64 `json:"dag_replanned"`
	LegacyReplanned        float64 `json:"legacy_replanned"`
	DAGEventsObserved      float64 `json:"dag_events_observed"`
	LegacyEventsObserved   float64 `json:"legacy_events_observed"`
}

// Report is the deterministic promotion decision and its supporting values.
type Report struct {
	Decision             string   `json:"decision"`
	Window               string   `json:"window"`
	Snapshot             Snapshot `json:"snapshot"`
	DAGNonSuccessRate    float64  `json:"dag_non_success_rate"`
	LegacyNonSuccessRate float64  `json:"legacy_non_success_rate"`
	P95LatencyRatio      float64  `json:"p95_latency_ratio"`
	DAGApprovalRate      float64  `json:"dag_approval_rate"`
	LegacyApprovalRate   float64  `json:"legacy_approval_rate"`
	DAGReplanRate        float64  `json:"dag_replan_rate"`
	LegacyReplanRate     float64  `json:"legacy_replan_rate"`
	ManualReviewPassed   bool     `json:"manual_review_passed"`
	Reasons              []string `json:"reasons"`
}

// QueryClient reads one scalar result from a Prometheus instant query.
type QueryClient interface {
	Query(ctx context.Context, query string) (float64, error)
}

// PrometheusClient implements QueryClient against the Prometheus HTTP API.
type PrometheusClient struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// NewPrometheusClient validates the endpoint and returns a read-only query client.
func NewPrometheusClient(rawURL string, httpClient *http.Client) (*PrometheusClient, error) {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Prometheus URL %q", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported Prometheus URL scheme %q", parsed.Scheme)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &PrometheusClient{baseURL: parsed, httpClient: httpClient}, nil
}

// Query executes an instant query. Empty vectors are treated as zero samples.
func (c *PrometheusClient) Query(ctx context.Context, query string) (float64, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query"
	values := endpoint.Query()
	values.Set("query", query)
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("build Prometheus query: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query Prometheus: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Prometheus query returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Status    string `json:"status"`
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Data      struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if payload.Status != "success" {
		return 0, fmt.Errorf("Prometheus query failed (%s): %s", payload.ErrorType, payload.Error)
	}
	if len(payload.Data.Result) == 0 {
		return 0, nil
	}
	if len(payload.Data.Result) != 1 || len(payload.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("Prometheus query returned an unexpected result shape")
	}
	var rawValue string
	if err := json.Unmarshal(payload.Data.Result[0].Value[1], &rawValue); err != nil {
		return 0, fmt.Errorf("decode Prometheus scalar value: %w", err)
	}
	value, err := strconv.ParseFloat(rawValue, 64)
	if err != nil {
		return 0, fmt.Errorf("parse Prometheus scalar value %q: %w", rawValue, err)
	}
	return value, nil
}

// Collect reads all values needed for a promotion decision.
func Collect(ctx context.Context, client QueryClient, window string) (Snapshot, error) {
	queries := []struct {
		name  string
		query string
		set   func(*Snapshot, float64)
	}{
		{"DAG calls", runtimeCallsQuery("dag", "", window), func(s *Snapshot, v float64) { s.DAGCalls = v }},
		{"DAG successes", runtimeCallsQuery("dag", "success", window), func(s *Snapshot, v float64) { s.DAGSuccesses = v }},
		{"Legacy calls", runtimeCallsQuery("legacy", "", window), func(s *Snapshot, v float64) { s.LegacyCalls = v }},
		{"Legacy successes", runtimeCallsQuery("legacy", "success", window), func(s *Snapshot, v float64) { s.LegacySuccesses = v }},
		{"DAG fallbacks", counterDeltaQuery("agent_multiagent_runtime_fallbacks_total", "", window), func(s *Snapshot, v float64) { s.DAGFallbacks = v }},
		{"DAG P95 latency", latencyP95Query("dag", window), func(s *Snapshot, v float64) { s.DAGP95LatencyMS = v }},
		{"Legacy P95 latency", latencyP95Query("legacy", window), func(s *Snapshot, v float64) { s.LegacyP95LatencyMS = v }},
		{"DAG approval required", runtimeEventQuery("dag", "approval_required", window), func(s *Snapshot, v float64) { s.DAGApprovalRequired = v }},
		{"Legacy approval required", runtimeEventQuery("legacy", "approval_required", window), func(s *Snapshot, v float64) { s.LegacyApprovalRequired = v }},
		{"DAG replanned", runtimeEventQuery("dag", "replanned", window), func(s *Snapshot, v float64) { s.DAGReplanned = v }},
		{"Legacy replanned", runtimeEventQuery("legacy", "replanned", window), func(s *Snapshot, v float64) { s.LegacyReplanned = v }},
		{"DAG event coverage", runtimeEventQuery("dag", "observed", window), func(s *Snapshot, v float64) { s.DAGEventsObserved = v }},
		{"Legacy event coverage", runtimeEventQuery("legacy", "observed", window), func(s *Snapshot, v float64) { s.LegacyEventsObserved = v }},
	}
	var snapshot Snapshot
	for _, item := range queries {
		value, err := client.Query(ctx, item.query)
		if err != nil {
			return Snapshot{}, fmt.Errorf("query %s: %w", item.name, err)
		}
		item.set(&snapshot, value)
	}
	return snapshot, nil
}

// Evaluate applies the release thresholds without changing runtime configuration.
func Evaluate(cfg Config, snapshot Snapshot) Report {
	report := Report{
		Decision:           "PROMOTE",
		Window:             cfg.Window,
		Snapshot:           snapshot,
		ManualReviewPassed: cfg.ManualReviewPassed,
		Reasons:            []string{},
	}
	report.DAGNonSuccessRate = nonSuccessRate(snapshot.DAGCalls, snapshot.DAGSuccesses)
	report.LegacyNonSuccessRate = nonSuccessRate(snapshot.LegacyCalls, snapshot.LegacySuccesses)
	report.DAGApprovalRate = eventRate(snapshot.DAGCalls, snapshot.DAGApprovalRequired)
	report.LegacyApprovalRate = eventRate(snapshot.LegacyCalls, snapshot.LegacyApprovalRequired)
	report.DAGReplanRate = eventRate(snapshot.DAGCalls, snapshot.DAGReplanned)
	report.LegacyReplanRate = eventRate(snapshot.LegacyCalls, snapshot.LegacyReplanned)
	if finitePositive(snapshot.DAGP95LatencyMS) && finitePositive(snapshot.LegacyP95LatencyMS) {
		report.P95LatencyRatio = snapshot.DAGP95LatencyMS / snapshot.LegacyP95LatencyMS
	}

	if snapshot.DAGSuccesses < cfg.MinDAGSuccesses {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG successes %.0f < required %.0f", snapshot.DAGSuccesses, cfg.MinDAGSuccesses))
	}
	if snapshot.LegacyCalls < cfg.MinLegacyCalls {
		report.Reasons = append(report.Reasons, fmt.Sprintf("Legacy calls %.0f < required %.0f", snapshot.LegacyCalls, cfg.MinLegacyCalls))
	}
	if snapshot.DAGEventsObserved < snapshot.DAGCalls {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG runtime-event coverage %.0f < calls %.0f", snapshot.DAGEventsObserved, snapshot.DAGCalls))
	}
	if snapshot.LegacyEventsObserved < snapshot.LegacyCalls {
		report.Reasons = append(report.Reasons, fmt.Sprintf("Legacy runtime-event coverage %.0f < calls %.0f", snapshot.LegacyEventsObserved, snapshot.LegacyCalls))
	}
	if snapshot.DAGFallbacks > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG fallbacks %.0f > 0", snapshot.DAGFallbacks))
	}
	if snapshot.DAGCalls > 0 && snapshot.LegacyCalls > 0 && report.DAGNonSuccessRate > report.LegacyNonSuccessRate+cfg.MaxNonSuccessRateDelta {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG non-success rate %.4f exceeds Legacy %.4f by more than %.4f", report.DAGNonSuccessRate, report.LegacyNonSuccessRate, cfg.MaxNonSuccessRateDelta))
	}
	if snapshot.DAGCalls > 0 && snapshot.LegacyCalls > 0 && report.DAGApprovalRate > report.LegacyApprovalRate+cfg.MaxApprovalRateDelta {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG approval-required rate %.4f exceeds Legacy %.4f by more than %.4f", report.DAGApprovalRate, report.LegacyApprovalRate, cfg.MaxApprovalRateDelta))
	}
	if snapshot.DAGCalls > 0 && snapshot.LegacyCalls > 0 && report.DAGReplanRate > report.LegacyReplanRate+cfg.MaxReplanRateDelta {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG replan rate %.4f exceeds Legacy %.4f by more than %.4f", report.DAGReplanRate, report.LegacyReplanRate, cfg.MaxReplanRateDelta))
	}
	if !finitePositive(snapshot.DAGP95LatencyMS) || !finitePositive(snapshot.LegacyP95LatencyMS) {
		report.Reasons = append(report.Reasons, "comparable DAG and Legacy P95 latency samples are unavailable")
	} else if report.P95LatencyRatio > cfg.MaxP95LatencyRatio {
		report.Reasons = append(report.Reasons, fmt.Sprintf("DAG/Legacy P95 ratio %.4f > allowed %.4f", report.P95LatencyRatio, cfg.MaxP95LatencyRatio))
	}
	if !cfg.ManualReviewPassed {
		report.Reasons = append(report.Reasons, "approval and Replan behavior requires manual review")
	}
	if len(report.Reasons) > 0 {
		report.Decision = "HOLD"
	}
	return report
}

func runtimeCallsQuery(runtime, outcome, window string) string {
	labels := fmt.Sprintf("runtime=%q", runtime)
	if outcome != "" {
		labels += fmt.Sprintf(",outcome=%q", outcome)
	}
	return counterDeltaQuery("agent_multiagent_runtime_calls_total", labels, window)
}

// counterDeltaQuery avoids increase() extrapolation when a series is younger
// than the observation window. A counter reset produces a conservative zero
// instead of an inflated promotion sample count.
func counterDeltaQuery(metric, labels, window string) string {
	selector := metric
	if labels != "" {
		selector += "{" + labels + "}"
	}
	return fmt.Sprintf("clamp_min(sum(%s) - (sum(%s offset %s) or vector(0)), 0)", selector, selector, window)
}

func latencyP95Query(runtime, window string) string {
	return fmt.Sprintf("histogram_quantile(0.95, sum by (le) (increase(agent_multiagent_runtime_latency_ms_bucket{runtime=%q}[%s])))", runtime, window)
}

func runtimeEventQuery(runtime, event, window string) string {
	return counterDeltaQuery("agent_multiagent_runtime_events_total", fmt.Sprintf("runtime=%q,event=%q", runtime, event), window)
}

func nonSuccessRate(calls, successes float64) float64 {
	if calls <= 0 {
		return 0
	}
	return math.Max(0, calls-successes) / calls
}

func eventRate(calls, events float64) float64 {
	if calls <= 0 {
		return 0
	}
	return events / calls
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
