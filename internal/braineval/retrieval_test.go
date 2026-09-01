package braineval

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/types"
)

var scopeAtlas = Scope{TenantID: "tenant-north", ProjectID: "project-atlas"}

func TestMergeRRF_DeduplicatesByCanonicalURI(t *testing.T) {
	branches := [][]Candidate{
		{{URI: "memory://m-1", Branch: "memory", Rank: 1}},
		{{URI: "memory://m-1", Branch: "brain", Rank: 2}, {URI: "wiki://atlas/projects/current", Branch: "brain", Rank: 1}},
	}

	got := MergeRRF(branches, 60)
	if len(got) != 2 || got[0].URI != "memory://m-1" {
		t.Fatalf("got %#v", got)
	}
	wantScore := 1.0/61.0 + 1.0/62.0
	if math.Abs(got[0].Score-wantScore) > 1e-15 {
		t.Fatalf("score = %v, want %v", got[0].Score, wantScore)
	}
}

func TestMergeRRF_BreaksEqualScoresByURI(t *testing.T) {
	got := MergeRRF([][]Candidate{{
		{URI: "wiki://atlas/projects/zeta", Rank: 1},
		{URI: "wiki://atlas/projects/alpha", Rank: 1},
	}}, 60)
	if len(got) != 2 || got[0].URI != "wiki://atlas/projects/alpha" {
		t.Fatalf("got %#v", got)
	}
}

func TestSelectEvidence_EnforcesItemsAndUTF8Bytes(t *testing.T) {
	in := []Candidate{
		{URI: "memory://1", Snippet: strings.Repeat("界", 3000)},
		{URI: "memory://2", Snippet: "second"},
	}
	got, err := SelectEvidence(context.Background(), fakeRetriever{}, scopeAtlas, in, 3, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %#v", len(got), got)
	}
	content := strings.Join(got[0].Lines, "\n")
	if len([]byte(content)) > 8000 || !utf8.ValidString(content) {
		t.Fatalf("budget violation: %d bytes, valid=%t", len([]byte(content)), utf8.ValidString(content))
	}
	if len([]byte(content)) != 7998 {
		t.Fatalf("truncated length = %d, want 7998", len([]byte(content)))
	}
}

func TestSelectEvidence_StopsAtItemLimit(t *testing.T) {
	in := []Candidate{
		{URI: "memory://1", Snippet: "one"},
		{URI: "memory://2", Snippet: "two"},
		{URI: "memory://3", Snippet: "three"},
		{URI: "memory://4", Snippet: "four"},
	}
	got, err := SelectEvidence(context.Background(), fakeRetriever{}, scopeAtlas, in, 3, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestSelectEvidence_ClampsCallerLimitsToFinalBudget(t *testing.T) {
	items := []Candidate{
		{URI: "memory://1", Snippet: "one"},
		{URI: "memory://2", Snippet: "two"},
		{URI: "memory://3", Snippet: "three"},
		{URI: "memory://4", Snippet: "four"},
	}
	got, err := SelectEvidence(context.Background(), fakeRetriever{}, scopeAtlas, items, 99, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != FinalEvidenceLimit {
		t.Fatalf("len = %d, want global limit %d", len(got), FinalEvidenceLimit)
	}

	oversized := []Candidate{{URI: "memory://large", Snippet: strings.Repeat("界", 3000)}}
	got, err = SelectEvidence(context.Background(), fakeRetriever{}, scopeAtlas, oversized, 99, 9000)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Join(got[0].Lines, "\n")
	if len(content) > FinalEvidenceBytes {
		t.Fatalf("content = %d bytes, want at most %d", len(content), FinalEvidenceBytes)
	}
}

func TestMemoryRetriever_IsolatesScopeAndDropsZeroOverlap(t *testing.T) {
	atlas := projectCorpus("tenant-north", "project-atlas", "atlas", "task-atlas", "2026-09-02T10:00:00Z")
	atlas.Memories = []MemoryFixture{{
		ID: "atlas-owner", SessionID: "session-project-atlas", TaskID: "task-atlas", RecordedAt: "2026-09-02T11:00:00Z",
		Goal: "Find project owner", FinalAnswer: "Atlas owner is Mei", KeyFindings: []string{"Mei owns Atlas"},
	}}
	orbit := projectCorpus("tenant-north", "project-orbit", "orbit", "task-orbit", "2026-09-02T10:00:00Z")
	orbit.Memories = []MemoryFixture{{
		ID: "orbit-secret", SessionID: "session-project-orbit", TaskID: "task-orbit", RecordedAt: "2026-09-02T11:00:00Z",
		Goal: "Orbit launch code", FinalAnswer: "The launch code is seven", KeyFindings: []string{"Orbit secret"},
	}}
	south := projectCorpus("tenant-south", "project-atlas", "south", "task-south", "2026-09-02T10:00:00Z")
	south.Sessions[0].ID = "session-south-atlas"
	south.Memories = []MemoryFixture{{
		ID: "south-secret", SessionID: "session-south-atlas", TaskID: "task-south", RecordedAt: "2026-09-02T11:00:00Z",
		Goal: "Southern launch code", FinalAnswer: "The launch code is nine", KeyFindings: []string{"South secret"},
	}}

	retriever, err := NewMemoryRetriever(context.Background(), corpusWithProjects(atlas, orbit, south))
	if err != nil {
		t.Fatal(err)
	}
	got, err := retriever.Search(context.Background(), scopeAtlas, "owner", 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URI != "memory://atlas-owner" || got[0].Branch != "memory" || got[0].Rank != 1 {
		t.Fatalf("atlas search = %#v", got)
	}

	got, err = retriever.Search(context.Background(), scopeAtlas, "launch code", BranchCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-scope or zero-overlap memories leaked: %#v", got)
	}
	if _, err := retriever.Fetch(context.Background(), scopeAtlas, Candidate{URI: "memory://orbit-secret"}); err == nil {
		t.Fatal("expected cross-project memory fetch to fail")
	}

	evidence, err := retriever.Fetch(context.Background(), scopeAtlas, Candidate{URI: "memory://atlas-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Path != "memory://atlas-owner" || !strings.Contains(strings.Join(evidence.Lines, "\n"), "Mei owns Atlas") {
		t.Fatalf("evidence = %#v", evidence)
	}
}

func TestMemoryRetriever_CapsBranchAtEightAndOrdersTiesByURI(t *testing.T) {
	project := projectCorpus("tenant-north", "project-atlas", "atlas", "task-atlas", "2026-09-02T10:00:00Z")
	for i := 9; i >= 0; i-- {
		project.Memories = append(project.Memories, MemoryFixture{
			ID: string(rune('a' + i)), SessionID: "session-project-atlas", TaskID: "task-atlas", RecordedAt: "2026-09-02T11:00:00Z",
			Goal: "common", FinalAnswer: "common", KeyFindings: []string{"common"},
		})
	}
	retriever, err := NewMemoryRetriever(context.Background(), corpusWithProjects(project))
	if err != nil {
		t.Fatal(err)
	}
	got, err := retriever.Search(context.Background(), scopeAtlas, "common", 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != BranchCandidateLimit {
		t.Fatalf("len = %d, want %d: %#v", len(got), BranchCandidateLimit, got)
	}
	for i, candidate := range got {
		if candidate.Rank != i+1 {
			t.Fatalf("candidate[%d] rank = %d, want %d", i, candidate.Rank, i+1)
		}
		if i > 0 && got[i-1].URI >= candidate.URI {
			t.Fatalf("candidates are not ordered by URI: %#v", got)
		}
	}
}

func TestWikiRetriever_IsolatesScopeAndRevalidatesURI(t *testing.T) {
	atlasRoot := writeBrainRoot(t, "# Atlas\n\nOwner Mei runs Atlas.\n")
	orbitRoot := writeBrainRoot(t, "# Orbit\n\nLaunch code seven is secret.\n")
	southRoot := writeBrainRoot(t, "# South\n\nLaunch code nine is secret.\n")

	atlas := projectCorpus("tenant-north", "project-atlas", "atlas", "task-atlas", "2026-09-02T10:00:00Z")
	atlas.Fixture.Root = atlasRoot
	orbit := projectCorpus("tenant-north", "project-orbit", "orbit", "task-orbit", "2026-09-02T10:00:00Z")
	orbit.Fixture.Root = orbitRoot
	south := projectCorpus("tenant-south", "project-atlas", "south", "task-south", "2026-09-02T10:00:00Z")
	south.Sessions[0].ID = "session-south-atlas"
	south.Fixture.Root = southRoot

	retriever, err := NewWikiRetriever(context.Background(), corpusWithProjects(atlas, orbit, south))
	if err != nil {
		t.Fatal(err)
	}
	got, err := retriever.Search(context.Background(), scopeAtlas, "owner mei", 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URI != "wiki://atlas/projects/current" || got[0].Branch != "brain" || got[0].Rank != 1 {
		t.Fatalf("atlas search = %#v", got)
	}
	got, err = retriever.Search(context.Background(), scopeAtlas, "launch code", BranchCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-scope Wiki pages leaked: %#v", got)
	}
	if _, err := retriever.Fetch(context.Background(), scopeAtlas, Candidate{URI: "wiki://orbit/projects/current"}); err == nil {
		t.Fatal("expected cross-space Wiki fetch to fail")
	}

	evidence, err := retriever.Fetch(context.Background(), scopeAtlas, Candidate{URI: "wiki://atlas/projects/current"})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Path != "wiki://atlas/projects/current" || !strings.Contains(strings.Join(evidence.Lines, "\n"), "Owner Mei") {
		t.Fatalf("evidence = %#v", evidence)
	}
}

func TestWikiRetriever_SearchSkipsDirectoryNavigationPages(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	if err := os.MkdirAll(brain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, "_index.md"), []byte("# Navigation\n\nnavonlymarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	atlas := projectCorpus("tenant-north", "project-atlas", "atlas", "task-atlas", "2026-09-02T10:00:00Z")
	atlas.Fixture.Root = root
	retriever, err := NewWikiRetriever(context.Background(), corpusWithProjects(atlas))
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := retriever.Search(context.Background(), scopeAtlas, "navonlymarker", BranchCandidateLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("navigation page returned as evidence: %#v", candidates)
	}
}

func TestWikiRetriever_SearchRejectsNonNavigationInvalidURI(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	if err := os.MkdirAll(brain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, "orphan.md"), []byte("# Orphan\n\ninvalidonlymarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	atlas := projectCorpus("tenant-north", "project-atlas", "atlas", "task-atlas", "2026-09-02T10:00:00Z")
	atlas.Fixture.Root = root
	retriever, err := NewWikiRetriever(context.Background(), corpusWithProjects(atlas))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retriever.Search(context.Background(), scopeAtlas, "invalidonlymarker", BranchCandidateLimit); err == nil {
		t.Fatal("expected non-navigation invalid URI to remain fail-closed")
	}
}

type fakeRetriever struct{}

func (fakeRetriever) Search(context.Context, Scope, string, int) ([]Candidate, error) {
	return nil, nil
}

func (fakeRetriever) Fetch(_ context.Context, _ Scope, candidate Candidate) (types.Evidence, error) {
	return types.Evidence{Path: candidate.URI, Lines: []string{candidate.Snippet}}, nil
}

func writeBrainRoot(t *testing.T, page string) string {
	t.Helper()
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	if err := os.MkdirAll(filepath.Join(brain, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, "_index.md"), []byte("# Index\n\n- [Current](projects/current.md)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, "projects", "current.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
