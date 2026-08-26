package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"
)

// WikiMetricsSnapshot is the process-local view exposed by /api/metrics. The
// same events are also emitted through OpenTelemetry below.
type WikiMetricsSnapshot struct {
	BackendCalls            int64   `json:"backend_calls"`
	BackendErrors           int64   `json:"backend_errors"`
	BackendAverageLatencyMS float64 `json:"backend_average_latency_ms"`
	BackendP95LatencyMS     float64 `json:"backend_p95_latency_ms"`
	CircuitOpened           int64   `json:"circuit_opened"`
	CircuitRejected         int64   `json:"circuit_rejected"`
	ReadinessFailures       int64   `json:"readiness_failures"`
	CandidateCacheTasks     int64   `json:"candidate_cache_tasks"`
	CandidateCacheReleased  int64   `json:"candidate_cache_released"`
	CandidateCacheExpired   int64   `json:"candidate_cache_expired"`
	CandidateCacheEvicted   int64   `json:"candidate_cache_evicted"`
}

var wikiLocalMetrics struct {
	backendCalls           atomic.Int64
	backendErrors          atomic.Int64
	backendLatencyNS       atomic.Int64
	latencyBuckets         [9]atomic.Int64
	circuitOpened          atomic.Int64
	circuitRejected        atomic.Int64
	readinessFailures      atomic.Int64
	candidateCacheTasks    atomic.Int64
	candidateCacheReleased atomic.Int64
	candidateCacheExpired  atomic.Int64
	candidateCacheEvicted  atomic.Int64
}

// CurrentWikiMetrics returns a race-safe process-local Wiki metrics snapshot.
func CurrentWikiMetrics() WikiMetricsSnapshot {
	calls := wikiLocalMetrics.backendCalls.Load()
	average := float64(0)
	if calls > 0 {
		average = float64(wikiLocalMetrics.backendLatencyNS.Load()) / float64(time.Millisecond) / float64(calls)
	}
	return WikiMetricsSnapshot{
		BackendCalls: calls, BackendErrors: wikiLocalMetrics.backendErrors.Load(),
		BackendAverageLatencyMS: average, BackendP95LatencyMS: wikiLatencyP95MS(calls), CircuitOpened: wikiLocalMetrics.circuitOpened.Load(),
		CircuitRejected:        wikiLocalMetrics.circuitRejected.Load(),
		ReadinessFailures:      wikiLocalMetrics.readinessFailures.Load(),
		CandidateCacheTasks:    wikiLocalMetrics.candidateCacheTasks.Load(),
		CandidateCacheReleased: wikiLocalMetrics.candidateCacheReleased.Load(),
		CandidateCacheExpired:  wikiLocalMetrics.candidateCacheExpired.Load(),
		CandidateCacheEvicted:  wikiLocalMetrics.candidateCacheEvicted.Load(),
	}
}

var (
	wikiMeter                     = otel.Meter("ai-agent/wiki")
	wikiBackendCalls, _           = wikiMeter.Int64Counter("agent.wiki.backend.calls")
	wikiBackendErrors, _          = wikiMeter.Int64Counter("agent.wiki.backend.errors")
	wikiBackendLatency, _         = wikiMeter.Float64Histogram("agent.wiki.backend.latency_ms")
	wikiCircuitOpened, _          = wikiMeter.Int64Counter("agent.wiki.circuit.opened")
	wikiCircuitRejected, _        = wikiMeter.Int64Counter("agent.wiki.circuit.rejected")
	wikiReadinessFailures, _      = wikiMeter.Int64Counter("agent.wiki.readiness.failures")
	wikiCandidateCacheTasks, _    = wikiMeter.Int64UpDownCounter("agent.wiki.candidate_cache.tasks")
	wikiCandidateCacheRemovals, _ = wikiMeter.Int64Counter("agent.wiki.candidate_cache.removals")
)

var wikiCacheOwners = struct {
	sync.Mutex
	byTask map[string]map[*wikiCache]struct{}
}{byTask: make(map[string]map[*wikiCache]struct{})}

func registerWikiCacheOwner(taskKey string, cache *wikiCache) {
	wikiCacheOwners.Lock()
	defer wikiCacheOwners.Unlock()
	owners := wikiCacheOwners.byTask[taskKey]
	if owners == nil {
		owners = make(map[*wikiCache]struct{})
		wikiCacheOwners.byTask[taskKey] = owners
	}
	owners[cache] = struct{}{}
}

func unregisterWikiCacheOwner(taskKey string, cache *wikiCache) {
	wikiCacheOwners.Lock()
	defer wikiCacheOwners.Unlock()
	owners := wikiCacheOwners.byTask[taskKey]
	delete(owners, cache)
	if len(owners) == 0 {
		delete(wikiCacheOwners.byTask, taskKey)
	}
}

// ReleaseWikiTaskCache actively drops task-local Wiki candidate IDs after a
// task reaches a terminal state. It is idempotent and tenant-scoped.
func ReleaseWikiTaskCache(taskID, tenantID string) {
	taskKey := strings.TrimSpace(tenantID) + "\x00" + strings.TrimSpace(taskID)
	if strings.TrimSpace(taskID) == "" {
		return
	}
	wikiCacheOwners.Lock()
	owners := make([]*wikiCache, 0, len(wikiCacheOwners.byTask[taskKey]))
	for cache := range wikiCacheOwners.byTask[taskKey] {
		owners = append(owners, cache)
	}
	wikiCacheOwners.Unlock()
	for _, cache := range owners {
		cache.release(taskKey)
	}
}

func observeWikiCacheTaskAdded() {
	wikiLocalMetrics.candidateCacheTasks.Add(1)
	wikiCandidateCacheTasks.Add(context.Background(), 1)
}

func observeWikiCacheRemoval(reason string) {
	wikiLocalMetrics.candidateCacheTasks.Add(-1)
	switch reason {
	case "terminal":
		wikiLocalMetrics.candidateCacheReleased.Add(1)
	case "expired":
		wikiLocalMetrics.candidateCacheExpired.Add(1)
	case "capacity":
		wikiLocalMetrics.candidateCacheEvicted.Add(1)
	}
	wikiCandidateCacheTasks.Add(context.Background(), -1)
	wikiCandidateCacheRemovals.Add(context.Background(), 1, api.WithAttributes(attribute.String("reason", reason)))
}

// ObserveWikiReadinessFailure records a required-Wiki readiness failure.
func ObserveWikiReadinessFailure(ctx context.Context) {
	wikiLocalMetrics.readinessFailures.Add(1)
	wikiReadinessFailures.Add(ctx, 1)
}

type wikiBackendGuard struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	halfOpen  bool
}

func (g *wikiBackendGuard) call(ctx context.Context, operation string, invoke func() error) error {
	if err := g.before(ctx, operation); err != nil {
		return err
	}
	started := time.Now()
	err := invoke()
	elapsed := time.Since(started)
	wikiLocalMetrics.backendCalls.Add(1)
	wikiLocalMetrics.backendLatencyNS.Add(elapsed.Nanoseconds())
	wikiLocalMetrics.latencyBuckets[wikiLatencyBucket(elapsed)].Add(1)
	attrs := []attribute.KeyValue{attribute.String("operation", operation)}
	wikiBackendCalls.Add(ctx, 1, api.WithAttributes(attrs...))
	wikiBackendLatency.Record(ctx, float64(elapsed)/float64(time.Millisecond), api.WithAttributes(attrs...))
	if err != nil {
		wikiLocalMetrics.backendErrors.Add(1)
		wikiBackendErrors.Add(ctx, 1, api.WithAttributes(attrs...))
	}
	g.after(ctx, operation, err)
	return err
}

var wikiLatencyBounds = [...]time.Duration{
	10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2500 * time.Millisecond,
}

func wikiLatencyBucket(elapsed time.Duration) int {
	for index, bound := range wikiLatencyBounds {
		if elapsed <= bound {
			return index
		}
	}
	return len(wikiLatencyBounds)
}

func wikiLatencyP95MS(calls int64) float64 {
	if calls <= 0 {
		return 0
	}
	target := (calls*95 + 99) / 100
	var cumulative int64
	for index := range wikiLocalMetrics.latencyBuckets {
		cumulative += wikiLocalMetrics.latencyBuckets[index].Load()
		if cumulative >= target {
			if index < len(wikiLatencyBounds) {
				return float64(wikiLatencyBounds[index]) / float64(time.Millisecond)
			}
			return 2500
		}
	}
	return 0
}

func (g *wikiBackendGuard) before(ctx context.Context, operation string) error {
	threshold, cooldown := wikiCircuitSettings()
	if threshold == 0 || cooldown == 0 {
		return nil
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.openUntil) {
		wikiLocalMetrics.circuitRejected.Add(1)
		wikiCircuitRejected.Add(ctx, 1, api.WithAttributes(attribute.String("operation", operation)))
		return fmt.Errorf("wiki backend circuit is open; retry after %s", time.Until(g.openUntil).Round(time.Millisecond))
	}
	if !g.openUntil.IsZero() {
		if g.halfOpen {
			wikiLocalMetrics.circuitRejected.Add(1)
			wikiCircuitRejected.Add(ctx, 1, api.WithAttributes(attribute.String("operation", operation)))
			return fmt.Errorf("wiki backend circuit is half-open; probe in progress")
		}
		g.halfOpen = true
	}
	return nil
}

func (g *wikiBackendGuard) after(ctx context.Context, operation string, callErr error) {
	threshold, cooldown := wikiCircuitSettings()
	g.mu.Lock()
	defer g.mu.Unlock()
	if threshold == 0 || cooldown == 0 {
		g.failures = 0
		g.openUntil = time.Time{}
		g.halfOpen = false
		return
	}
	if callErr == nil {
		g.failures = 0
		g.openUntil = time.Time{}
		g.halfOpen = false
		return
	}
	g.halfOpen = false
	g.failures++
	if g.failures >= threshold {
		g.openUntil = time.Now().Add(cooldown)
		wikiLocalMetrics.circuitOpened.Add(1)
		wikiCircuitOpened.Add(ctx, 1, api.WithAttributes(attribute.String("operation", operation)))
	}
}

func wikiCircuitSettings() (int, time.Duration) {
	wiki := config.Get().Wiki
	if wiki.CircuitBreakerFailureThreshold <= 0 || wiki.CircuitBreakerCooldownSeconds <= 0 {
		return 0, 0
	}
	return wiki.CircuitBreakerFailureThreshold, time.Duration(wiki.CircuitBreakerCooldownSeconds) * time.Second
}
