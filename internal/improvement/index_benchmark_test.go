package improvement

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/harness"
)

func benchmarkLessonStore(b *testing.B, rows int) (*Store, Lesson) {
	b.Helper()
	s := NewStore(b.TempDir())
	var latest Lesson
	for i := 0; i < rows; i++ {
		team := fmt.Sprintf("team-%02d", i%100)
		l, err := s.Record(b.Context(), Lesson{Team: team, HarnessBefore: "base", HarnessAfter: fmt.Sprint(i), Edits: []harness.Edit{{Target: "teams." + team + ".planner.model", Op: "set", After: fmt.Sprint(i)}}, ObservedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)})
		if err != nil {
			b.Fatal(err)
		}
		if i%10 == 0 {
			if err := s.Validate(b.Context(), Validation{LessonID: l.ID, ReportSHA256: strings.Repeat("a", 64), DatasetSHA256: strings.Repeat("b", 64), Cases: 2, MaxLatencyRatio: 1.2, MaxCostRatio: 1.2, MaxTaskCostUSD: .1, MaxLLMCalls: 20}); err != nil {
				b.Fatal(err)
			}
			if err := s.Publish(b.Context(), l.ID, "benchmark fixture"); err != nil {
				b.Fatal(err)
			}
		}
		if team == "team-00" {
			latest = l
		}
	}
	return s, latest
}

func BenchmarkLessonLookup(b *testing.B) {
	for _, rows := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("lessons_%d", rows), func(b *testing.B) {
			store, latest := benchmarkLessonStore(b, rows)
			for _, mode := range []string{"team_8", "published_team_1", "published_cold"} {
				b.Run(mode, func(b *testing.B) {
					lookup := func() {
						// Production creates short-lived Store handles; they must share the index.
						s := NewStore(store.root)
						var result []Lesson
						var err error
						if mode == "team_8" {
							result, err = s.List(b.Context(), "team-00", 8)
						} else {
							result, err = s.ListPublished(b.Context(), "team-00", 1)
						}
						if err != nil || len(result) == 0 || result[0].ID != latest.ID {
							b.Fatalf("lookup failed: %v", err)
						}
					}
					lookup() // Warm filesystem and, when available, the process-local index.
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if mode == "published_cold" {
							b.StopTimer()
							if _, err := store.Record(b.Context(), latest); err != nil {
								b.Fatal(err)
							}
							b.StartTimer()
						}
						lookup()
					}
				})
			}
		})
	}
}
