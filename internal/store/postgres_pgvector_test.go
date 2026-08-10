package store

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestPGVectorLiteral(t *testing.T) {
	got := pgVectorLiteral([]float32{1, 0.25, -3.5})
	want := "[1,0.25,-3.5]"
	if got != want {
		t.Fatalf("pgVectorLiteral() = %q, want %q", got, want)
	}
}

func TestPGVectorLiteralEmpty(t *testing.T) {
	got := pgVectorLiteral(nil)
	want := "[]"
	if got != want {
		t.Fatalf("pgVectorLiteral(nil) = %q, want %q", got, want)
	}
}

func TestFuseMemoryRankingsPromotesOverlap(t *testing.T) {
	now := time.Now().UTC()
	a := &types.Memory{ID: "a", Timestamp: now}
	b := &types.Memory{ID: "b", Timestamp: now}
	c := &types.Memory{ID: "c", Timestamp: now}

	got := fuseMemoryRankings([][]*types.Memory{{a, b}, {c, b}}, 3, 0, now)
	if len(got) != 3 || got[0].ID != "b" {
		t.Fatalf("fused ranking = %+v, want overlapping memory b first", got)
	}
}

func TestFuseMemoryRankingsDeduplicatesAndLimits(t *testing.T) {
	now := time.Now().UTC()
	a := &types.Memory{ID: "a", Timestamp: now}
	b := &types.Memory{ID: "b", Timestamp: now.Add(-time.Hour)}

	got := fuseMemoryRankings([][]*types.Memory{{a, a, b}}, 1, 0, now)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("fused ranking = %+v, want one deduplicated result a", got)
	}
	if got := fuseMemoryRankings(nil, 0, 0, now); len(got) != 0 {
		t.Fatalf("zero-limit fused ranking = %+v, want empty", got)
	}
}

func TestPostgresTextNormalizesInvalidUTF8(t *testing.T) {
	valid := "already valid 中文"
	if got := postgresText(valid); got != valid {
		t.Fatalf("valid text changed: %q", got)
	}
	got := postgresText("before\xc2\nafter")
	if !utf8.ValidString(got) || got != "before\uFFFD\nafter" {
		t.Fatalf("postgresText() = %q", got)
	}
}
