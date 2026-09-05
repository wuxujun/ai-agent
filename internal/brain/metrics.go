package brain

import (
	"context"
	"sync"
	"time"
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
}
func ObservePublish(_ context.Context, outcome, corpus string) {
	brainMetrics.Lock()
	brainMetrics.PublishTotal++
	if outcome == "conflict" {
		brainMetrics.PublishConflicts++
	}
	brainMetrics.Unlock()
}
func ObserveRetractionBlocked(_ context.Context, corpus string) {
	brainMetrics.Lock()
	brainMetrics.RetractionBlocked++
	brainMetrics.Unlock()
}
func ObserveSearch(_ context.Context, corpus, outcome string) {
	brainMetrics.Lock()
	brainMetrics.SearchTotal++
	brainMetrics.Unlock()
}
func ObserveFetch(_ context.Context, corpus, outcome string) {
	brainMetrics.Lock()
	brainMetrics.FetchTotal++
	brainMetrics.Unlock()
}
func ObserveCacheHit(_ context.Context, corpus string) {
	brainMetrics.Lock()
	brainMetrics.CacheHits++
	brainMetrics.Unlock()
}
func ObserveSnapshotAge(age time.Duration) {
	brainMetrics.Lock()
	brainMetrics.SnapshotAgeSeconds = age.Seconds()
	brainMetrics.Unlock()
}
