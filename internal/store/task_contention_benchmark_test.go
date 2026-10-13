package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// A deliberately bursty, bounded workload: one writer and 0/4/16 observers.
// Each observer performs b.N projections; b.N counts saves, not all operations.
// This is an in-process store test, not an HTTP/SSE connection or HA soak test.
func BenchmarkTaskConcurrentCapacity(b *testing.B) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, rows := range []int{1000, 10000} {
			for _, readers := range []int{0, 4, 16} {
				b.Run(fmt.Sprintf("%s/traces_%d/readers_%d", backend, rows, readers), func(b *testing.B) {
					st, path := capacityStore(b, backend)
					task := capacityTask(rows)
					task.StepCount = 0
					if err := st.(TaskCreationStore).CreateTask(b.Context(), task); err != nil {
						b.Fatal(err)
					}
					projection := st.(TaskStreamSnapshotStore)
					id := task.ID
					ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
					defer cancel()
					readTimes := make([][]int64, readers)
					for i := range readTimes {
						readTimes[i] = make([]int64, 0, b.N)
					}
					writeTimes := make([]int64, 0, b.N)
					var failures []error
					var mu sync.Mutex
					failed := func(err error) { mu.Lock(); failures = append(failures, err); mu.Unlock(); cancel() }
					var workers sync.WaitGroup
					start := make(chan struct{})
					workers.Add(1)
					go func() {
						defer workers.Done()
						<-start
						for i := 0; i < b.N; i++ {
							task.StepCount = i + 1
							task.Trace[rows-1].TokenUsage.TotalTokens = 15 + (i+1)%2
							before := time.Now()
							err := st.SaveFullTask(ctx, task)
							writeTimes = append(writeTimes, time.Since(before).Nanoseconds())
							if err != nil {
								failed(err)
								return
							}
						}
					}()
					for n := 0; n < readers; n++ {
						workers.Add(1)
						go func(n int) {
							defer workers.Done()
							<-start
							for i := 0; i < b.N; i++ {
								before := time.Now()
								got, err := projection.GetTaskStreamSnapshot(ctx, id)
								readTimes[n] = append(readTimes[n], time.Since(before).Nanoseconds())
								if err != nil {
									failed(err)
									return
								}
								if got.TraceCount != rows || got.Status != types.StatusRunning || got.TokenUsage.TotalTokens != rows*15+got.StepCount%2 {
									failed(fmt.Errorf("mixed task and Trace observation"))
									return
								}
							}
						}(n)
					}
					var waitsBefore int64
					var waitBefore time.Duration
					if sqlStore, ok := st.(*SQLiteStore); ok {
						stats := sqlStore.db.Stats()
						waitsBefore, waitBefore = stats.WaitCount, stats.WaitDuration
					}
					b.ReportAllocs()
					b.ResetTimer()
					began := time.Now()
					close(start)
					workers.Wait()
					elapsed := time.Since(began)
					b.StopTimer()
					if len(failures) > 0 {
						b.Fatalf("contention run failed: %v", failures[0])
					}
					if len(writeTimes) != b.N {
						b.Fatal("writer did not finish its requested operations")
					}
					sample := func(values []int64, metric string) {
						if len(values) == 0 {
							return
						}
						sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
						b.ReportMetric(float64(values[(len(values)*95+99)/100-1]), metric)
					}
					allReads := []int64{}
					for _, values := range readTimes {
						if len(values) != b.N {
							b.Fatal("observer did not finish")
						}
						allReads = append(allReads, values...)
					}
					sample(allReads, "read-p95-ns")
					sample(writeTimes, "write-p95-ns")
					total := b.N * (readers + 1)
					b.ReportMetric(float64(total)/elapsed.Seconds(), "total-ops/s")
					b.ReportMetric(float64(len(allReads)), "reads")
					b.ReportMetric(float64(b.N), "writes")
					if sqlStore, ok := st.(*SQLiteStore); ok {
						stats := sqlStore.db.Stats()
						b.ReportMetric(float64(stats.WaitCount-waitsBefore), "pool-waits")
						b.ReportMetric(float64((stats.WaitDuration-waitBefore).Nanoseconds())/float64(total), "pool-wait-ns/op")
						b.ReportMetric(float64(capacityDiskBytes(path)), "db-wal-bytes")
					}
				})
			}
		}
	}
}
