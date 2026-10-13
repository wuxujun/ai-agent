package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// Offline synthetic fixtures only. No external store, provider or service state.
func capacityStore(b *testing.B, backend string) (Store, string) {
	b.Helper()
	if backend == "memory" {
		return NewMemoryStore(), ""
	}
	path := filepath.Join(b.TempDir(), "capacity.db")
	st, err := NewSQLiteStore(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := st.Close(); err != nil {
			b.Error(err)
		}
	})
	return st, path
}
func capacityTask(rows int) *types.Task {
	task := &types.Task{ID: "capacity-task", TenantID: "capacity-tenant", Goal: "Synthetic capacity fixture", Status: types.StatusRunning, StepCount: rows, CreatedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	for i := 0; i < rows; i++ {
		task.Trace = append(task.Trace, types.StepTrace{Step: i/3 + 1, Action: "read_file", Observation: fmt.Sprintf("row-%06d ", i) + strings.Repeat("x", 1000), TokenUsage: types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, Evidence: []types.Evidence{{Lines: []string{"synthetic evidence"}}}})
	}
	return task
}
func capacityMeasure(b *testing.B, call func() error) {
	b.Helper()
	samples := make([]int64, 0, min(b.N, 10000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		err := call()
		elapsed := time.Since(start).Nanoseconds()
		if err != nil {
			b.Fatal(err)
		}
		if len(samples) < cap(samples) {
			samples = append(samples, elapsed)
		}
	}
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)*95+99)/100-1]), "p95-ns")
		b.ReportMetric(float64(len(samples)), "samples")
	}
}
func capacityDiskBytes(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}
func BenchmarkTaskReadCapacity(b *testing.B) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, rows := range []int{100, 1000, 10000} {
			b.Run(fmt.Sprintf("%s/traces_%d", backend, rows), func(b *testing.B) {
				st, path := capacityStore(b, backend)
				task := capacityTask(rows)
				ctx := context.Background()
				if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
					b.Fatal(err)
				}
				body, err := json.Marshal(task)
				if err != nil {
					b.Fatal(err)
				}
				logicalBytes := len(body)
				diskBytes := capacityDiskBytes(path)
				operations := []struct {
					name string
					call func() error
				}{
					{"full", func() error {
						got, err := st.GetTask(ctx, task.ID)
						if err == nil && len(got.Trace) != rows {
							return fmt.Errorf("full trace count mismatch")
						}
						return err
					}},
					{"stream_snapshot", func() error {
						got, err := st.(TaskStreamSnapshotStore).GetTaskStreamSnapshot(ctx, task.ID)
						if err == nil && (got.TraceCount != rows || got.TokenUsage.TotalTokens != rows*15 || got.StepCount != rows) {
							return fmt.Errorf("stream snapshot mismatch")
						}
						return err
					}},
					{"metadata", func() error {
						got, err := st.(TaskSummaryStore).GetTaskWithoutTrace(ctx, task.ID)
						if err == nil && (len(got.Trace) != 0 || got.StepCount != rows) {
							return fmt.Errorf("metadata mismatch")
						}
						return err
					}},
					{"trace_first_100", func() error {
						got, err := st.(TaskTraceStore).ListTaskTraces(ctx, task.ID, 0, 100)
						if err == nil && (len(got) != 100 || got[0].Sequence != 1) {
							return fmt.Errorf("first page mismatch")
						}
						return err
					}},
					{"trace_tail_100", func() error {
						got, err := st.(TaskTraceStore).ListTaskTraces(ctx, task.ID, int64(rows-100), 100)
						if err == nil && (len(got) != 100 || got[0].Sequence != int64(rows-99)) {
							return fmt.Errorf("tail page mismatch")
						}
						return err
					}},
				}
				for _, op := range operations {
					b.Run(op.name, func(b *testing.B) {
						capacityMeasure(b, op.call)
						b.ReportMetric(float64(logicalBytes), "fixture-json-bytes")
						if path != "" {
							b.ReportMetric(float64(diskBytes), "db-wal-bytes")
						}
					})
				}
			})
		}
	}
}
func BenchmarkTaskSummaryCapacity(b *testing.B) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, count := range []int{100, 10000} {
			b.Run(fmt.Sprintf("%s/tasks_%d", backend, count), func(b *testing.B) {
				st, _ := capacityStore(b, backend)
				ctx := context.Background()
				created := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
				for i := 0; i < count; i++ {
					task := &types.Task{ID: fmt.Sprintf("task-%06d", i), TenantID: "capacity-tenant", Status: types.StatusRunning, CreatedAt: created.Add(time.Duration(i) * time.Second), Goal: "Synthetic summary"}
					if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
						b.Fatal(err)
					}
				}
				reader := st.(TaskSummaryStore)
				for _, depth := range []int{0, count - 20} {
					b.Run(fmt.Sprintf("offset_equivalent_%d", depth), func(b *testing.B) {
						filter := TaskSummaryFilter{TenantID: "capacity-tenant", Limit: 20}
						if depth > 0 {
							filter.BeforeID = fmt.Sprintf("task-%06d", count-depth)
							filter.BeforeCreatedAt = created.Add(time.Duration(count-depth) * time.Second)
						}
						capacityMeasure(b, func() error {
							got, err := reader.ListTaskSummaries(ctx, filter)
							if err == nil && (len(got) != 20 || got[0].ID != fmt.Sprintf("task-%06d", count-depth-1)) {
								return fmt.Errorf("summary page mismatch")
							}
							return err
						})
					})
				}
			})
		}
	}
}
func BenchmarkTaskSnapshotWriteCapacity(b *testing.B) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, rows := range []int{100, 1000, 10000} {
			b.Run(fmt.Sprintf("%s/traces_%d", backend, rows), func(b *testing.B) {
				st, path := capacityStore(b, backend)
				ctx := context.Background()
				task := capacityTask(rows)
				if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
					b.Fatal(err)
				}
				// Fixed-size unchanged snapshot: separates rewrite cost from growing fixtures.
				capacityMeasure(b, func() error { return st.SaveFullTask(ctx, task) })
				if path != "" {
					b.ReportMetric(float64(capacityDiskBytes(path)), "db-wal-bytes")
				}
			})
		}
	}
}

// Complements the unchanged-snapshot baseline with one real row mutation.
func BenchmarkTaskSnapshotChangedWriteCapacity(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("sqlite/traces_%d", rows), func(b *testing.B) {
			st, path := capacityStore(b, "sqlite")
			ctx := context.Background()
			task := capacityTask(rows)
			if err := st.(TaskCreationStore).CreateTask(ctx, task); err != nil {
				b.Fatal(err)
			}
			revision := 0
			capacityMeasure(b, func() error {
				revision++
				task.Trace[len(task.Trace)-1].Observation = fmt.Sprintf("revision-%d ", revision) + strings.Repeat("x", 1000)
				return st.SaveFullTask(ctx, task)
			})
			b.ReportMetric(float64(capacityDiskBytes(path)), "db-wal-bytes")
		})
	}
}
