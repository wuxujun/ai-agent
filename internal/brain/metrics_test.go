package brain

import (
	"context"
	"testing"
)

func TestBrainMetricsSnapshotCountsBoundedEvents(t *testing.T) {
	before := CurrentMetrics()
	ObserveSearch(context.Background(), "brain", "success")
	ObserveFetch(context.Background(), "brain", "error")
	ObserveCacheHit(context.Background(), "brain")
	after := CurrentMetrics()
	if after.SearchTotal != before.SearchTotal+1 || after.FetchTotal != before.FetchTotal+1 || after.CacheHits != before.CacheHits+1 {
		t.Fatalf("metrics delta = before=%+v after=%+v", before, after)
	}
}

func TestBrainMetricsExcludeHighCardinalityLabels(t *testing.T) {
	// The public counter API accepts only bounded corpus/outcome values; IDs are
	// intentionally absent from its contract.
	ObservePublish(context.Background(), "success", "brain")
	if got := CurrentMetrics().PublishTotal; got < 1 {
		t.Fatalf("publish total = %d", got)
	}
}

func TestBrainMetricsCountsPublishConflictAndRetraction(t *testing.T) {
	before := CurrentMetrics()
	ObservePublish(context.Background(), "conflict", "brain")
	ObserveRetractionBlocked(context.Background(), "brain")
	after := CurrentMetrics()
	if after.PublishConflicts != before.PublishConflicts+1 || after.RetractionBlocked != before.RetractionBlocked+1 {
		t.Fatalf("metrics delta = before=%+v after=%+v", before, after)
	}
}
