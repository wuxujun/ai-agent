package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// BenchmarkFuseMemoryRankings provides a stable CPU/allocation baseline for
// tuning ParadeDB candidate multiplier and RRF K independently of database
// network and query-plan variance.
func BenchmarkFuseMemoryRankings(b *testing.B) {
	now := time.Now().UTC()
	for _, candidatesPerRanking := range []int{20, 100, 400} {
		rankings := benchmarkRankings(candidatesPerRanking, now)
		for _, rrfK := range []float64{30, 60, 90} {
			name := fmt.Sprintf("candidates_%d/k_%g", candidatesPerRanking, rrfK)
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					result := fuseMemoryRankings(rankings, 10, 0, rrfK, now)
					if len(result) != 10 {
						b.Fatalf("result count = %d, want 10", len(result))
					}
				}
			})
		}
	}
}

func benchmarkRankings(size int, now time.Time) [][]*types.Memory {
	first := make([]*types.Memory, 0, size)
	second := make([]*types.Memory, 0, size)
	for i := 0; i < size; i++ {
		first = append(first, &types.Memory{ID: fmt.Sprintf("memory-%d", i), Timestamp: now.Add(-time.Duration(i) * time.Minute)})
		// Half overlap exercises the promotion and deduplication path.
		second = append(second, &types.Memory{ID: fmt.Sprintf("memory-%d", i+size/2), Timestamp: now.Add(-time.Duration(i) * time.Minute)})
	}
	return [][]*types.Memory{first, second}
}
