package brain

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

type MetricsSnapshot struct {
	CompileTotal, PublishTotal, PublishConflicts, RetractionBlocked int64
	SearchTotal, FetchTotal, CacheHits                              int64
	CompileAverageDurationMS, SnapshotAgeSeconds                    float64
}

var brainMetrics struct {
	sync.Mutex
	MetricsSnapshot
	compileDuration time.Duration
	compileCount    int64
}

var brainMeter = otel.Meter("ai-agent/brain")
var (
	compileInstrument, _         = brainMeter.Int64Counter("agent.brain.compile.total")
	publishInstrument, _         = brainMeter.Int64Counter("agent.brain.publish.total")
	searchInstrument, _          = brainMeter.Int64Counter("agent.brain.search.total")
	fetchInstrument, _           = brainMeter.Int64Counter("agent.brain.fetch.total")
	cacheInstrument, _           = brainMeter.Int64Counter("agent.brain.cache.hits")
	retractionInstrument, _      = brainMeter.Int64Counter("agent.brain.retraction.blocked")
	compileDurationInstrument, _ = brainMeter.Float64Histogram("agent.brain.compile.duration_ms")
	snapshotAgeInstrument, _     = brainMeter.Float64Histogram("agent.brain.snapshot.age_seconds")
)

func brainAttrs(outcome, corpus string) metricapi.MeasurementOption {
	return metricapi.WithAttributes(attribute.String("outcome", outcome), attribute.String("corpus", corpus), attribute.String("provider", "brain"))
}

func CurrentMetrics() MetricsSnapshot {
	brainMetrics.Lock()
	defer brainMetrics.Unlock()
	snapshot := brainMetrics.MetricsSnapshot
	if brainMetrics.compileCount > 0 {
		snapshot.CompileAverageDurationMS = float64(brainMetrics.compileDuration.Milliseconds()) / float64(brainMetrics.compileCount)
	}
	return snapshot
}

func ObserveCompile(_ context.Context, outcome string, duration time.Duration) {
	brainMetrics.Lock()
	brainMetrics.CompileTotal++
	brainMetrics.compileCount++
	brainMetrics.compileDuration += duration
	brainMetrics.Unlock()
	compileInstrument.Add(context.Background(), 1, brainAttrs(outcome, "brain"))
	compileDurationInstrument.Record(context.Background(), float64(duration.Microseconds())/1000, brainAttrs(outcome, "brain"))
}
func ObservePublish(_ context.Context, outcome, corpus string) {
	brainMetrics.Lock()
	brainMetrics.PublishTotal++
	if outcome == "conflict" {
		brainMetrics.PublishConflicts++
	}
	brainMetrics.Unlock()
	publishInstrument.Add(context.Background(), 1, brainAttrs(outcome, corpus))
}
func ObserveRetractionBlocked(_ context.Context, corpus string) {
	brainMetrics.Lock()
	brainMetrics.RetractionBlocked++
	brainMetrics.Unlock()
	retractionInstrument.Add(context.Background(), 1, brainAttrs("blocked", corpus))
}
func ObserveSearch(_ context.Context, corpus, outcome string) {
	brainMetrics.Lock()
	brainMetrics.SearchTotal++
	brainMetrics.Unlock()
	searchInstrument.Add(context.Background(), 1, brainAttrs(outcome, corpus))
}
func ObserveFetch(_ context.Context, corpus, outcome string) {
	brainMetrics.Lock()
	brainMetrics.FetchTotal++
	brainMetrics.Unlock()
	fetchInstrument.Add(context.Background(), 1, brainAttrs(outcome, corpus))
}
func ObserveCacheHit(_ context.Context, corpus string) {
	brainMetrics.Lock()
	brainMetrics.CacheHits++
	brainMetrics.Unlock()
	cacheInstrument.Add(context.Background(), 1, brainAttrs("hit", corpus))
}
func ObserveSnapshotAge(age time.Duration) {
	brainMetrics.Lock()
	brainMetrics.SnapshotAgeSeconds = age.Seconds()
	brainMetrics.Unlock()
	snapshotAgeInstrument.Record(context.Background(), age.Seconds(), brainAttrs("observed", "brain"))
}
