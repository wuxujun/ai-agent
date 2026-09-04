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

func TestRenderCanonicalizesPermutedPagesClaimsEvidenceAndLinks(t *testing.T) {
	canonical, err := Render(renderSynthesis(), 4000)
	if err != nil {
		t.Fatal(err)
	}
	permuted := renderSynthesis()
	permuted.Pages[0], permuted.Pages[1] = permuted.Pages[1], permuted.Pages[0]
	permuted.Pages[0].Claims[0].EvidenceIDs[0], permuted.Pages[0].Claims[0].EvidenceIDs[1] = permuted.Pages[0].Claims[0].EvidenceIDs[1], permuted.Pages[0].Claims[0].EvidenceIDs[0]
	permuted.Pages[0].Claims[0], permuted.Pages[0].Claims[1] = permuted.Pages[0].Claims[1], permuted.Pages[0].Claims[0]
	permuted.Pages[0].Links = append(permuted.Pages[0].Links, "wiki://brain-atlas/projects/roadmap")
	permuted.Pages[0].Links[0], permuted.Pages[0].Links[1] = permuted.Pages[0].Links[1], permuted.Pages[0].Links[0]
	canonicalWithLinks := renderSynthesis()
	canonicalWithLinks.Pages[1].Links = []string{"wiki://brain-atlas/entities/widget", "wiki://brain-atlas/projects/roadmap"}
	want, err := Render(canonicalWithLinks, 4000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Render(permuted, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(canonical, got) || !reflect.DeepEqual(want, got) {
		t.Fatalf("permuted render was not canonical: got=%q want=%q", got.Files["concepts/alpha.md"], want.Files["concepts/alpha.md"])
	}
	page := string(got.Files["concepts/alpha.md"])
	if strings.Index(page, "### claim-a") > strings.Index(page, "### claim-z") || strings.Index(page, "evidence-a, evidence-z") < 0 || strings.Index(page, "entities/widget") > strings.Index(page, "projects/roadmap") {
		t.Fatalf("page ordering is not canonical: %q", page)
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
