package braineval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSource_ReturnsTypedUnknownAndCrossScopeErrors(t *testing.T) {
	scopeA := Scope{TenantID: "tenant-a", ProjectID: "atlas"}
	scopeB := Scope{TenantID: "tenant-b", ProjectID: "atlas"}
	sources := map[string][]corpusSource{
		"task://known": {{scope: scopeA, uri: "task://known"}},
	}
	if _, err := resolveSource(sources, "task://missing", scopeA); !errors.Is(err, ErrUnknownEvidenceURI) {
		t.Fatalf("unknown error = %v, want ErrUnknownEvidenceURI", err)
	}
	if _, err := resolveSource(sources, "task://known", scopeB); !errors.Is(err, ErrCrossScopeEvidenceURI) {
		t.Fatalf("cross-scope error = %v, want ErrCrossScopeEvidenceURI", err)
	}
}

func TestParseEvidenceURI_RequiresAbsoluteWikiURI(t *testing.T) {
	for _, raw := range []string{"wiki://local/projects/atlas", "session://s-001", "task://t-001", "memory://m-001"} {
		if _, err := ParseEvidenceURI(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"wiki://projects/atlas", "file:///tmp/source", "task://", "memory://m/extra", "task://not canonical", "task://control\x00byte"} {
		if _, err := ParseEvidenceURI(raw); err == nil {
			t.Fatalf("expected malformed URI %q to fail", raw)
		}
	}
}

func TestFixtureTypes_UseSnakeCaseJSONFields(t *testing.T) {
	encoded, err := json.Marshal(struct {
		Evidence EvidenceRef   `json:"evidence"`
		Project  ProjectCorpus `json:"project"`
		Corpus   Corpus        `json:"corpus"`
	}{
		Evidence: EvidenceRef{Scheme: "wiki", Space: "atlas", Kind: "projects", ID: "current"},
		Project:  ProjectCorpus{},
		Corpus:   Corpus{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"scheme"`, `"space"`, `"kind"`, `"id"`, `"fixture"`, `"sessions"`, `"memories"`, `"retractions"`, `"claims"`, `"projects"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("expected snake_case JSON field %s in %s", field, encoded)
		}
	}
}

func TestParseGoldClaims_RejectsMalformedEvidenceCitation(t *testing.T) {
	content := []byte("- Current owner is Mei Lin. [evidence](task://owner) [evidence](task://bad uri)\n")
	if _, err := parseGoldClaims(content, Scope{TenantID: "tenant-a", ProjectID: "atlas"}, "wiki://atlas/projects/current"); err == nil || !strings.Contains(err.Error(), "malformed evidence citation") {
		t.Fatalf("expected malformed evidence citation error, got %v", err)
	}
}

func TestParseGoldClaims_RejectsEvidenceOnlyClaim(t *testing.T) {
	content := []byte("- [evidence](task://owner)\n")
	if _, err := parseGoldClaims(content, Scope{TenantID: "tenant-a", ProjectID: "atlas"}, "wiki://atlas/projects/current"); err == nil || !strings.Contains(err.Error(), "claim text must not be empty") {
		t.Fatalf("expected empty Gold claim text error, got %v", err)
	}
}

func TestCorpusValidate_RejectsInvalidProvenance(t *testing.T) {
	for _, tt := range []struct {
		name    string
		corpus  *Corpus
		wantErr string
	}{
		{name: "superseding evidence is older", corpus: corpusWithClaims(
			claim("Current deadline", "task://new", "task://old"),
		), wantErr: "timeline"},
		{name: "supersedes another scope", corpus: corpusWithProjects(
			projectCorpus("tenant-a", "atlas", "atlas", "old", "2026-09-02T10:00:00Z"),
			projectCorpus("tenant-b", "orbit", "orbit", "new", "2026-09-03T10:00:00Z", claim("Current deadline", "task://new", "task://old")),
		), wantErr: "cross-scope"},
		{name: "retraction predates source", corpus: corpusWithRetraction("task://new", "2026-09-01T09:00:00Z"), wantErr: "retraction timestamp"},
		{name: "retraction has unknown source", corpus: corpusWithRetraction("task://missing", "2026-09-03T09:00:00Z"), wantErr: "unknown retraction URI"},
		{name: "claim cites retracted evidence", corpus: corpusWithClaims(claim("Current deadline", "task://new", "")), wantErr: "retracted evidence"},
		{name: "claim cites another scope", corpus: corpusWithProjects(
			projectCorpus("tenant-a", "atlas", "atlas", "foreign", "2026-09-02T10:00:00Z"),
			projectCorpus("tenant-b", "orbit", "orbit", "local", "2026-09-03T10:00:00Z", claim("Current owner", "task://foreign", "")),
		), wantErr: "cross-scope"},
		{name: "canonical URI is shared across scopes", corpus: corpusWithCrossScopeURISharing(), wantErr: "cross-scope URI sharing"},
		{name: "claim requires evidence", corpus: corpusWithProjects(
			projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z", GoldClaim{Text: "Unsupported claim"}),
		), wantErr: "at least one evidence"},
		{name: "claim text must not be empty", corpus: corpusWithProjects(
			projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z", GoldClaim{EvidenceURIs: []string{"task://new"}}),
		), wantErr: "claim text must not be empty"},
		{name: "claim evidence URI is malformed", corpus: corpusWithMalformedClaimEvidence(), wantErr: "claim evidence URI"},
		{name: "supersedes URI is malformed", corpus: corpusWithMalformedSupersedes(), wantErr: "supersedes URI"},
		{name: "memory references missing session", corpus: corpusWithMissingMemorySession(), wantErr: "unknown session"},
		{name: "memory references missing task", corpus: corpusWithMissingMemoryTask(), wantErr: "unknown task"},
		{name: "memory task belongs to another session", corpus: corpusWithMismatchedMemoryTask(), wantErr: "does not belong to session"},
		{name: "memory predates referenced session", corpus: corpusWithMemoryTimestamp("2026-09-01T08:00:00Z"), wantErr: "memory timestamp precedes session"},
		{name: "memory predates referenced task", corpus: corpusWithMemoryTimestamp("2026-09-01T09:30:00Z"), wantErr: "memory timestamp precedes task"},
		{name: "session ID must synthesize canonical URI", corpus: corpusWithInvalidSessionID(), wantErr: "session evidence URI"},
		{name: "memory ID must synthesize canonical URI", corpus: corpusWithInvalidMemoryID(), wantErr: "memory evidence URI"},
		{name: "task requires canonical evidence URI", corpus: corpusWithMissingTaskEvidenceURI(), wantErr: "task evidence URI"},
		{name: "task predates session", corpus: corpusWithTaskBeforeSession(), wantErr: "task timestamp"},
		{name: "invalid session RFC3339 timestamp", corpus: corpusWithInvalidSessionTimestamp(), wantErr: "invalid session timestamp"},
		{name: "invalid task RFC3339 timestamp", corpus: corpusWithInvalidTaskTimestamp(), wantErr: "invalid task timestamp"},
		{name: "invalid memory RFC3339 timestamp", corpus: corpusWithInvalidMemoryTimestamp(), wantErr: "invalid memory timestamp"},
		{name: "invalid retraction RFC3339 timestamp", corpus: corpusWithInvalidRetractionTimestamp(), wantErr: "invalid retraction timestamp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.corpus.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadCorpus_LoadsClaimsAndRejectsUnsafeCaseEvidence(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "fixtures", "atlas")
	writeValidFixture(t, root)
	dataset := fixtureDataset(root)
	corpus, err := LoadCorpus(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	project := corpus.Projects[dataset.Projects[0].Scope.Key()]
	if project == nil || len(project.Claims) != 2 {
		t.Fatalf("expected two Gold claims, got %#v", project)
	}
	if project.Claims[0].Text != "Project Atlas owner is Mei Lin." || project.Claims[1].Supersedes != "task://deadline-old" {
		t.Fatalf("unexpected parsed claims: %#v", project.Claims)
	}
	if got := project.Claims[0].EvidenceURIs; len(got) != 2 || got[0] != "memory://owner" || got[1] != "session://session-1" {
		t.Fatalf("expected all Markdown evidence links to be parsed, got %#v", got)
	}

	dataset.Cases[0].ForbiddenClaims = []string{"outside-scope fact remains text-only"}
	if _, err := LoadCorpus(context.Background(), dataset); err != nil {
		t.Fatalf("expected text-only forbidden claim to be accepted: %v", err)
	}
	dataset.Cases[0].ExpectedEvidenceURIs = []string{"task://not canonical"}
	if _, err := LoadCorpus(context.Background(), dataset); err == nil || !strings.Contains(err.Error(), "expected evidence URI") {
		t.Fatalf("expected malformed case evidence URI to fail, got %v", err)
	}
	dataset.Cases[0].ExpectedEvidenceURIs = []string{"task://outside"}
	if _, err := LoadCorpus(context.Background(), dataset); err == nil || !strings.Contains(err.Error(), "unknown evidence URI") {
		t.Fatalf("expected unsafe case evidence to fail, got %v", err)
	}
}

func TestLoadCorpus_RejectsOversizedIndexAndMalformedJSONL(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "fixtures", "atlas")
	writeValidFixture(t, root)
	if err := os.WriteFile(filepath.Join(root, "brain", "_index.md"), []byte(strings.Repeat("x", 4001)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(context.Background(), fixtureDataset(root)); err == nil || !strings.Contains(err.Error(), "_index.md exceeds") {
		t.Fatalf("expected oversized index failure, got %v", err)
	}
	writeValidFixture(t, root)
	if err := os.WriteFile(filepath.Join(root, "brain", "_index.md"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(context.Background(), fixtureDataset(root)); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("expected invalid UTF-8 index failure, got %v", err)
	}
	writeValidFixture(t, root)
	if err := os.WriteFile(filepath.Join(root, "sessions.jsonl"), []byte("{bad json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(context.Background(), fixtureDataset(root)); err == nil || !strings.Contains(err.Error(), "decode sessions.jsonl") {
		t.Fatalf("expected malformed JSONL failure, got %v", err)
	}
}

func fixtureDataset(root string) Dataset {
	return Dataset{
		Version:    SchemaVersion,
		Thresholds: Thresholds{0.10, 0.10, 1.50, 1.20},
		Projects:   []ProjectFixture{{Scope: Scope{TenantID: "tenant-a", ProjectID: "atlas"}, Space: "atlas", Root: root}},
		Cases:      []Case{{Name: "atlas-owner", Category: "decision", Scope: Scope{TenantID: "tenant-a", ProjectID: "atlas"}, Query: "owner", ExpectedClaims: []string{"Project Atlas owner is Mei Lin."}, ExpectedEvidenceURIs: []string{"memory://owner"}}},
	}
}

func writeValidFixture(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "brain", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"sessions.jsonl":            "{\"id\":\"session-1\",\"recorded_at\":\"2026-09-01T09:00:00Z\",\"tasks\":[{\"id\":\"deadline-old\",\"recorded_at\":\"2026-09-01T10:00:00Z\",\"summary\":\"old deadline\",\"evidence_uri\":\"task://deadline-old\"},{\"id\":\"deadline-new\",\"recorded_at\":\"2026-09-02T10:00:00Z\",\"summary\":\"new deadline\",\"evidence_uri\":\"task://deadline-new\"}]}\n",
		"memories.jsonl":            "{\"id\":\"owner\",\"session_id\":\"session-1\",\"task_id\":\"deadline-new\",\"recorded_at\":\"2026-09-02T11:00:00Z\",\"goal\":\"owner\",\"final_answer\":\"Mei Lin\",\"key_findings\":[\"Mei Lin owns Atlas\"]}\n",
		"retractions.jsonl":         "\n",
		"brain/_index.md":           "# Atlas\n\n- [Project](projects/current.md)\n",
		"brain/projects/current.md": "- Project Atlas owner is Mei Lin. [evidence](memory://owner) [evidence](session://session-1)\n- Current deadline is 2026-09-15. [evidence](task://deadline-new) supersedes: task://deadline-old\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func claim(text, evidence, supersedes string) GoldClaim {
	return GoldClaim{Text: text, EvidenceURIs: []string{evidence}, Supersedes: supersedes}
}

func corpusWithClaims(claims ...GoldClaim) *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Sessions[0].Tasks = append(p.Sessions[0].Tasks, TaskFixture{ID: "old", RecordedAt: "2026-09-03T10:00:00Z", EvidenceURI: "task://old"})
	for i := range claims {
		claims[i].Scope = p.Fixture.Scope
		claims[i].PageURI = "wiki://atlas/projects/current"
	}
	p.Claims = claims
	p.Retractions = []RetractionFixture{{URI: "task://new", RetractedAt: "2026-09-03T12:00:00Z", Reason: "replaced"}}
	if claims[0].Supersedes != "" {
		p.Retractions = nil
	}
	return corpusWithProjects(p)
}

func corpusWithRetraction(uri, retractedAt string) *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Retractions = []RetractionFixture{{URI: uri, RetractedAt: retractedAt, Reason: "bad"}}
	return corpusWithProjects(p)
}

func corpusWithMissingMemoryTask() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Memories = []MemoryFixture{{ID: "m", SessionID: "session-atlas", TaskID: "missing", RecordedAt: "2026-09-02T11:00:00Z"}}
	return corpusWithProjects(p)
}

func corpusWithMissingMemorySession() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Memories = []MemoryFixture{{ID: "m", SessionID: "missing", TaskID: "new", RecordedAt: "2026-09-02T11:00:00Z"}}
	return corpusWithProjects(p)
}

func corpusWithMismatchedMemoryTask() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "first", "2026-09-02T10:00:00Z")
	p.Sessions = append(p.Sessions, SessionFixture{
		ID:         "other",
		RecordedAt: "2026-09-02T11:00:00Z",
		Tasks: []TaskFixture{{
			ID:          "second",
			RecordedAt:  "2026-09-02T12:00:00Z",
			EvidenceURI: "task://second",
		}},
	})
	p.Memories = []MemoryFixture{{ID: "m", SessionID: "session-atlas", TaskID: "second", RecordedAt: "2026-09-02T13:00:00Z"}}
	return corpusWithProjects(p)
}

func corpusWithMemoryTimestamp(recordedAt string) *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-01T10:00:00Z")
	p.Memories = []MemoryFixture{{ID: "m", SessionID: "session-atlas", TaskID: "new", RecordedAt: recordedAt}}
	return corpusWithProjects(p)
}

func corpusWithInvalidSessionID() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-01T10:00:00Z")
	p.Sessions[0].ID = "bad/id"
	return corpusWithProjects(p)
}

func corpusWithInvalidMemoryID() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-01T10:00:00Z")
	p.Memories = []MemoryFixture{{ID: "bad/id", SessionID: "session-atlas", TaskID: "new", RecordedAt: "2026-09-01T11:00:00Z"}}
	return corpusWithProjects(p)
}

func corpusWithCrossScopeURISharing() *Corpus {
	return corpusWithProjects(
		projectCorpus("tenant-a", "atlas", "atlas", "shared", "2026-09-02T10:00:00Z"),
		projectCorpus("tenant-b", "orbit", "orbit", "shared", "2026-09-03T10:00:00Z"),
	)
}

func corpusWithMissingTaskEvidenceURI() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Sessions[0].Tasks[0].EvidenceURI = ""
	return corpusWithProjects(p)
}

func corpusWithMalformedClaimEvidence() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z", claim("Current owner", "file:///tmp/source", ""))
	return corpusWithProjects(p)
}

func corpusWithMalformedSupersedes() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z", claim("Current owner", "task://new", "file:///tmp/source"))
	return corpusWithProjects(p)
}

func corpusWithTaskBeforeSession() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-08-31T08:00:00Z")
	return corpusWithProjects(p)
}

func corpusWithInvalidSessionTimestamp() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Sessions[0].RecordedAt = "not-a-time"
	return corpusWithProjects(p)
}

func corpusWithInvalidTaskTimestamp() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "not-a-time")
	return corpusWithProjects(p)
}

func corpusWithInvalidMemoryTimestamp() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Memories = []MemoryFixture{{ID: "m", SessionID: "session-atlas", TaskID: "new", RecordedAt: "not-a-time"}}
	return corpusWithProjects(p)
}

func corpusWithInvalidRetractionTimestamp() *Corpus {
	p := projectCorpus("tenant-a", "atlas", "atlas", "new", "2026-09-02T10:00:00Z")
	p.Retractions = []RetractionFixture{{URI: "task://new", RetractedAt: "not-a-time", Reason: "bad"}}
	return corpusWithProjects(p)
}

func corpusWithProjects(projects ...*ProjectCorpus) *Corpus {
	corpus := &Corpus{Projects: make(map[string]*ProjectCorpus, len(projects))}
	for _, project := range projects {
		corpus.Projects[project.Fixture.Scope.Key()] = project
	}
	return corpus
}

func projectCorpus(tenant, project, space, taskID, taskTime string, claims ...GoldClaim) *ProjectCorpus {
	scope := Scope{TenantID: tenant, ProjectID: project}
	for i := range claims {
		claims[i].Scope = scope
		claims[i].PageURI = "wiki://" + space + "/projects/current"
	}
	return &ProjectCorpus{
		Fixture:  ProjectFixture{Scope: scope, Space: space, Root: "/fixture"},
		Sessions: []SessionFixture{{ID: "session-" + project, RecordedAt: "2026-09-01T09:00:00Z", Tasks: []TaskFixture{{ID: taskID, RecordedAt: taskTime, EvidenceURI: "task://" + taskID}}}},
		Claims:   claims,
	}
}
