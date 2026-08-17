package tools

import (
	"context"
	"fmt"
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
	CircuitOpened           int64   `json:"circuit_opened"`
	CircuitRejected         int64   `json:"circuit_rejected"`
}

var wikiLocalMetrics struct {
	backendCalls     atomic.Int64
	backendErrors    atomic.Int64
	backendLatencyNS atomic.Int64
	circuitOpened    atomic.Int64
	circuitRejected  atomic.Int64
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
		BackendAverageLatencyMS: average, CircuitOpened: wikiLocalMetrics.circuitOpened.Load(),
		CircuitRejected: wikiLocalMetrics.circuitRejected.Load(),
	}
}

var (
	wikiMeter              = otel.Meter("ai-agent/wiki")
	wikiBackendCalls, _    = wikiMeter.Int64Counter("agent.wiki.backend.calls")
	wikiBackendErrors, _   = wikiMeter.Int64Counter("agent.wiki.backend.errors")
	wikiBackendLatency, _  = wikiMeter.Float64Histogram("agent.wiki.backend.latency_ms")
	wikiCircuitOpened, _   = wikiMeter.Int64Counter("agent.wiki.circuit.opened")
	wikiCircuitRejected, _ = wikiMeter.Int64Counter("agent.wiki.circuit.rejected")
)

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
