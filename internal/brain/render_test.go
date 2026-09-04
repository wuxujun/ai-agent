package brain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenderIsByteStableAndBuildsCompactIndex(t *testing.T) {
	first, err := Render(renderSynthesis(), 4000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(renderSynthesis(), 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("render is not deterministic")
	}
	if len(first.Files["_index.md"]) > 4000 {
		t.Fatal("compact index exceeded limit")
	}
	page := string(first.Files["concepts/alpha.md"])
	if !strings.Contains(page, "[wiki://brain-atlas/entities/widget](wiki://brain-atlas/entities/widget) ([Markdown](../entities/widget.md))") {
		t.Fatalf("page lacks dual link: %q", page)
	}
	if strings.Contains(page, "\r") || !strings.HasSuffix(page, "\n") {
		t.Fatalf("page is not LF-normalized with one trailing newline: %q", page)
	}
}

func TestRenderRejectsUnsafePagePath(t *testing.T) {
	input := renderSynthesis()
	input.Pages[0].Slug = "../escape"
	if _, err := Render(input, 4000); err == nil {
		t.Fatal("Render accepted an unsafe path")
	}
}

func TestRenderRejectsIndexOverflow(t *testing.T) {
	if _, err := Render(renderSynthesis(), 10); err == nil {
		t.Fatal("Render accepted an index byte overflow")
	}
}

func renderSynthesis() Synthesis {
	when := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	return Synthesis{Pages: []Page{
		{
			Kind: "entities", Slug: "widget", Title: "Widget", Summary: "A related entity.",
			Claims: []Claim{{ID: "claim-widget", Text: "Widget is related.", Confidence: "high", State: "active", EvidenceIDs: []string{"evidence-b"}, ObservedAt: when}},
		},
		{
			Kind: "concepts", Slug: "alpha", Title: "Alpha", Summary: "A stable concept.",
			Links: []string{"wiki://brain-atlas/entities/widget"},
			Claims: []Claim{
				{ID: "claim-z", Text: "Later claim.", Confidence: "medium", State: "active", EvidenceIDs: []string{"evidence-z", "evidence-a"}, ObservedAt: when},
				{ID: "claim-a", Text: "Earlier claim.", Confidence: "high", State: "active", EvidenceIDs: []string{"evidence-a"}, ObservedAt: when},
			},
		},
	}}
}
