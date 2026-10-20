package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/refine"
)

func testRepoPaths(t *testing.T) (teamsYAML, skillsRoot string) {
	t.Helper()
	teamsYAML = filepath.Join("..", "..", "teams.yaml")
	skillsRoot = filepath.Join("..", "..", "skills")
	if _, err := os.Stat(teamsYAML); err != nil {
		t.Skipf("repo teams.yaml not available: %v", err)
	}
	return teamsYAML, skillsRoot
}

func TestMockEditorCandidates(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	base, err := harness.Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	general, domain, _, err := refine.MockEditor{}.Propose(t.Context(), base, "software", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(general) != 2 || len(domain) != 2 {
		t.Fatalf("want 2+2 candidates, got %d+%d", len(general), len(domain))
	}
	for i, es := range append(append([]harness.EditSet{}, general...), domain...) {
		if es.SnapshotID != base.ID {
			t.Fatalf("candidate %d: wrong base snapshot id", i)
		}
		if err := harness.Validate(es); err != nil {
			t.Fatalf("candidate %d invalid: %v", i, err)
		}
		if len(es.Edits) == 0 {
			t.Fatalf("candidate %d has no edits", i)
		}
	}
}

func TestMockEditorUnknownTeam(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	base, err := harness.Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, proposeErr := refine.MockEditor{}.Propose(t.Context(), base, "no-such-team", 1, nil)
	if proposeErr == nil {
		t.Fatal("expected error for unknown team")
	}
}

func TestProposeWritesFiles(t *testing.T) {
	teamsYAML, skillsRoot := testRepoPaths(t)
	out := t.TempDir()
	code := runPropose([]string{
		"--teams-yaml", teamsYAML, "--skills-root", skillsRoot,
		"--team", "software", "--k", "2", "--editor", "mock", "--out", out,
	}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("runPropose exit %d", code)
	}
	for _, name := range []string{"base-snapshot.json", "manifest.json", "teams.candidates.yaml",
		"candidate-00.json", "candidate-01.json", "candidate-02.json", "candidate-03.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	var m proposeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.BaseTeam != "software" || len(m.Candidates) != 4 {
		t.Fatalf("bad manifest: %+v", m)
	}
	if m.Candidates[0].Team != "software-cand-00" || m.Candidates[0].Kind != "general" {
		t.Fatalf("bad candidate entry: %+v", m.Candidates[0])
	}
	// Base snapshot must not be polluted with candidate teams.
	bsData, _ := os.ReadFile(filepath.Join(out, "base-snapshot.json"))
	var snap harness.Snapshot
	if err := json.Unmarshal(bsData, &snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Teams.Teams["software-cand-00"]; ok {
		t.Fatal("base snapshot polluted with candidate team")
	}
}
