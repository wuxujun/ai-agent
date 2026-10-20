package harness

import (
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
)

func testBase(t *testing.T) Snapshot {
	t.Helper()
	teams := multiagent.TeamsConfig{
		ActiveTeam: "software",
		Teams: map[string]multiagent.TeamConfig{
			"software": {
				Workflow: "planner_critic_executor_verifier",
				Planner: multiagent.AgentConfig{
					SystemPrompt: "You are a planner.",
					PromptName:   "teams/software/planner",
					PromptLabel:  "production",
					Tools:        []string{"find_files", "read_file"},
					Model:        "gemini-3.5-flash",
				},
				Critic: multiagent.AgentConfig{
					SystemPrompt: "You are a critic.",
					Tools:        []string{"read_file"},
				},
			},
		},
	}
	id, err := digestSnapshot(teams, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{ID: id, Source: "test", Teams: teams, SkillDigests: map[string]string{}}
}

func TestCaptureRealConfig(t *testing.T) {
	teamsYAML := filepath.Join("..", "..", "teams.yaml")
	skillsRoot := filepath.Join("..", "..", "skills")
	a, err := Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	b, err := Capture(teamsYAML, skillsRoot)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("digest not stable: %s != %s", a.ID, b.ID)
	}
	if len(a.Teams.Teams) == 0 {
		t.Fatal("expected teams in snapshot")
	}
	t.Logf("captured %d teams, digest %s", len(a.Teams.Teams), a.ID[:12])
}

func TestCaptureMissingFile(t *testing.T) {
	if _, err := Capture("testdata/does-not-exist.yaml", ""); err == nil {
		t.Fatal("expected error for missing teams.yaml")
	}
}

func TestApplySetAndDiffRoundTrip(t *testing.T) {
	base := testBase(t)
	es := EditSet{
		SnapshotID: base.ID,
		Author:     "test",
		Edits: []Edit{
			{Target: "teams.software.planner.system_prompt", Op: "set", After: "You are a senior planner."},
			{Target: "teams.software.planner.prompt_version", Op: "set", After: "3"},
			{Target: "teams.software.routing.reviewed_min_plan_steps", Op: "set", After: "5"},
			{Target: "teams.software.planner.tools", Op: "add-tools", After: `["search_text"]`},
		},
	}

	applied, err := Apply(base, es)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.ID == base.ID {
		t.Fatal("applied snapshot ID should differ from base")
	}
	got := applied.Teams.Teams["software"].Planner
	if got.SystemPrompt != "You are a senior planner." {
		t.Fatalf("system_prompt not applied: %q", got.SystemPrompt)
	}
	if got.PromptVersion != 3 {
		t.Fatalf("prompt_version not applied: %d", got.PromptVersion)
	}
	if applied.Teams.Teams["software"].Routing.ReviewedMinPlanSteps != 5 {
		t.Fatal("routing edit not applied")
	}
	if len(got.Tools) != 3 || got.Tools[2] != "search_text" {
		t.Fatalf("add-tools not applied: %v", got.Tools)
	}
	// base must be untouched
	if base.Teams.Teams["software"].Planner.SystemPrompt != "You are a planner." {
		t.Fatal("Apply mutated the base snapshot")
	}

	// Diff round-trip: re-applying the diff must reproduce the candidate.
	edits, err := Diff(base, applied)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(edits) == 0 {
		t.Fatal("expected non-empty diff")
	}
	replayed, err := Apply(base, EditSet{SnapshotID: base.ID, Edits: edits, Author: "replay"})
	if err != nil {
		t.Fatalf("Apply(diff): %v", err)
	}
	if replayed.ID != applied.ID {
		t.Fatalf("round-trip mismatch: %s != %s", replayed.ID, applied.ID)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []Edit{
		{Target: "teams.software.planner.workspace_root", Op: "set", After: "/tmp"},                 // not a field
		{Target: "teams.software.routing.allow_research_high_risk_tools", Op: "set", After: "true"}, // excluded
		{Target: "teams.software.planner.system_prompt", Op: "delete", After: "x"},                  // bad op
		{Target: "teams.software.planner.prompt_version", Op: "set", After: "notanint"},             // bad int
		{Target: "teams.software.planner.system_prompt", Op: "add-tools", After: `["x"]`},           // op/field mismatch
		{Target: "bad-target", Op: "set", After: "x"},                                               // bad grammar
		{Target: "teams.software.janitor.system_prompt", Op: "set", After: "x"},                     // bad role
	}
	for i, e := range cases {
		if err := Validate(EditSet{Edits: []Edit{e}}); err == nil {
			t.Fatalf("case %d (%s): expected validation error", i, e.Target)
		}
	}
}

func TestApplySnapshotIDMismatch(t *testing.T) {
	base := testBase(t)
	es := EditSet{
		SnapshotID: "deadbeef",
		Edits:      []Edit{{Target: "teams.software.planner.model", Op: "set", After: "x"}},
	}
	if _, err := Apply(base, es); err == nil {
		t.Fatal("expected snapshot ID mismatch error")
	}
}

func TestMutateToolsRemove(t *testing.T) {
	base := testBase(t)
	applied, err := Apply(base, EditSet{Edits: []Edit{
		{Target: "teams.software.planner.tools", Op: "remove-tools", After: `["read_file"]`},
	}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	tools := applied.Teams.Teams["software"].Planner.Tools
	if len(tools) != 1 || tools[0] != "find_files" {
		t.Fatalf("remove-tools wrong: %v", tools)
	}
}
