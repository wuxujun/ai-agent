package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

const benchmarkEmbeddingDim = 128

func BenchmarkQueryMemoriesMemoryStore(b *testing.B) {
	for _, size := range []int{200, 1000, 5000} {
		b.Run(fmt.Sprintf("rows_%d", size), func(b *testing.B) {
			ctx := context.Background()
			s := store.NewMemoryStore()
			seedBenchmarkMemories(b, ctx, s, size, benchmarkEmbeddingDim)
			queryEmbedding := benchmarkEmbedding(size-1, benchmarkEmbeddingDim)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.QueryMemories(ctx, "benchmark query", queryEmbedding, 5)
				if err != nil {
					b.Fatal(err)
				}
				if len(got) == 0 {
					b.Fatal("expected at least one memory")
				}
			}
		})
	}
}

func BenchmarkQueryMemoriesSQLiteDefaultCandidateLimit(b *testing.B) {
	ctx := context.Background()
	setMemoryCandidateLimitForBenchmark(b, 200)

	s, err := store.NewSQLiteStore(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("new SQLite store: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	seedBenchmarkMemories(b, ctx, s, 5000, benchmarkEmbeddingDim)
	queryEmbedding := benchmarkEmbedding(4999, benchmarkEmbeddingDim)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := s.QueryMemories(ctx, "benchmark query", queryEmbedding, 5)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) == 0 {
			b.Fatal("expected at least one memory")
		}
	}
}

func BenchmarkQueryMemoriesSQLiteFullCandidateScan(b *testing.B) {
	for _, size := range []int{200, 1000, 5000} {
		b.Run(fmt.Sprintf("rows_%d", size), func(b *testing.B) {
			ctx := context.Background()
			setMemoryCandidateLimitForBenchmark(b, size)

			s, err := store.NewSQLiteStore(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatalf("new SQLite store: %v", err)
			}
			b.Cleanup(func() { _ = s.Close() })

			seedBenchmarkMemories(b, ctx, s, size, benchmarkEmbeddingDim)
			queryEmbedding := benchmarkEmbedding(size-1, benchmarkEmbeddingDim)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.QueryMemories(ctx, "benchmark query", queryEmbedding, 5)
				if err != nil {
					b.Fatal(err)
				}
				if len(got) == 0 {
					b.Fatal("expected at least one memory")
				}
			}
		})
	}
}

type memorySaver interface {
	SaveMemory(context.Context, *types.Memory) error
}

func seedBenchmarkMemories(b *testing.B, ctx context.Context, s memorySaver, size int, dim int) {
	b.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < size; i++ {
		if err := s.SaveMemory(ctx, &types.Memory{
			ID:          fmt.Sprintf("mem-%06d", i),
			TaskID:      fmt.Sprintf("task-%06d", i),
			Goal:        fmt.Sprintf("benchmark memory goal %d", i),
			FinalAnswer: fmt.Sprintf("benchmark final answer %d", i),
			KeyFindings: fmt.Sprintf("benchmark key finding %d", i),
			Timestamp:   base.Add(time.Duration(i) * time.Second),
			Embedding:   benchmarkEmbedding(i, dim),
		}); err != nil {
			b.Fatalf("save memory %d: %v", i, err)
		}
	}
}

func benchmarkEmbedding(seed int, dim int) []float32 {
	embedding := make([]float32, dim)
	for i := range embedding {
		embedding[i] = float32(((seed+1)*(i+17))%101+1) / 101
	}
	return embedding
}

func setMemoryCandidateLimitForBenchmark(b *testing.B, limit int) {
	b.Helper()
	originalLimit := viper.GetInt("store.memory_candidate_limit")
	viper.Set("store.memory_candidate_limit", limit)
	if _, _, err := config.Reload(); err != nil {
		b.Fatalf("config reload: %v", err)
	}
	b.Cleanup(func() {
		viper.Set("store.memory_candidate_limit", originalLimit)
		_, _, _ = config.Reload()
	})
}
