package multiagent

import (
	"fmt"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRefineGateBucketed(t *testing.T) {
	// Disabled or zero percent never buckets.
	for _, g := range []RefineGate{
		{Enabled: false, Percent: 100, Team: "software", TaskID: "t1"},
		{Enabled: true, Percent: 0, Team: "software", TaskID: "t1"},
		{Enabled: true, Percent: 100, Team: "software", TaskID: ""},
		{Enabled: true, Percent: 101, Team: "software", TaskID: "t1"},
	} {
		if g.Bucketed() {
			t.Fatalf("gate %+v should not bucket", g)
		}
	}
	// 100% buckets every task with an ID.
	full := RefineGate{Enabled: true, Percent: 100, Team: "software", TaskID: "t1"}
	if !full.Bucketed() {
		t.Fatal("100% canary should bucket")
	}
}

func TestRefineGateDeterministic(t *testing.T) {
	g := RefineGate{Enabled: true, Percent: 50, Team: "software", TaskID: "stable-id"}
	first := g.Bucketed()
	for i := 0; i < 10; i++ {
		if g.Bucketed() != first {
			t.Fatal("bucketing must be deterministic per team+task")
		}
	}
}

func TestRefineGateNeverStacksWithDAG(t *testing.T) {
	// Configured DAG runtime: no refinement.
	if (RefineGate{Enabled: true, Percent: 100, Runtime: RuntimeDAG, Team: "s", TaskID: "t"}).Bucketed() {
		t.Fatal("refinement must not apply when the DAG runtime is configured")
	}
	// Task bucketed into the DAG canary keeps the base harness.
	// Find a task ID that lands in the DAG bucket at 50%.
	var dagTask string
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("dag-probe-%d", i)
		if stableRuntimeBucket("software", id) < 50 {
			dagTask = id
			break
		}
	}
	if dagTask == "" {
		t.Fatal("could not find a DAG-bucketed task id")
	}
	g := RefineGate{Enabled: true, Percent: 100, DAGPercent: 50, Runtime: RuntimeLegacy, Team: "software", TaskID: dagTask}
	if g.Bucketed() {
		t.Fatal("DAG-bucketed task must keep the base harness")
	}
}

func TestRefineSelectionPersisted(t *testing.T) {
	task := &types.Task{ID: "t1", Goal: "g"}
	AppendRefineSelection(task, "software", "lesson-1", "aaa", "bbb", 7, 10, false)
	sel, ok := PersistedRefineSelection(task, "software")
	if !ok {
		t.Fatal("expected persisted selection")
	}
	if sel.LessonID != "lesson-1" || sel.HarnessBefore != "aaa" || sel.HarnessAfter != "bbb" {
		t.Fatalf("bad selection: %+v", sel)
	}
	if sel.Source != "canary" {
		t.Fatalf("bad source: %q", sel.Source)
	}
	// Wrong team does not match.
	if _, ok := PersistedRefineSelection(task, "data"); ok {
		t.Fatal("selection must be team-scoped")
	}
}

func TestTeamOverlayIsolation(t *testing.T) {
	cfgA := TeamConfig{}
	cfgA.Planner.Model = "refined-model"
	SetTeamOverlay("task-a", "software", cfgA)
	defer ClearTeamOverlay("task-a")

	ov, ok := getTeamOverlay("task-a")
	if !ok || ov.cfg.Planner.Model != "refined-model" {
		t.Fatalf("overlay not registered: %+v %v", ov, ok)
	}
	if _, ok := getTeamOverlay("task-b"); ok {
		t.Fatal("overlay leaked to another task")
	}
	ClearTeamOverlay("task-a")
	if _, ok := getTeamOverlay("task-a"); ok {
		t.Fatal("overlay not cleared")
	}
	// Empty task ID is a no-op.
	SetTeamOverlay("", "software", cfgA)
	if _, ok := getTeamOverlay(""); ok {
		t.Fatal("empty task ID should not register")
	}
}

func TestApplyTeamOverlay(t *testing.T) {
	cfg := &TeamsConfig{
		ActiveTeam: "software",
		Teams:      map[string]TeamConfig{"software": {}},
	}
	refined := TeamConfig{}
	refined.Planner.SystemPrompt = "refined"
	SetTeamOverlay("task-x", "software", refined)
	defer ClearTeamOverlay("task-x")

	applyTeamOverlay("task-x", cfg)
	if cfg.Teams["software"].Planner.SystemPrompt != "refined" {
		t.Fatal("overlay not applied")
	}

	// Team mismatch: no-op.
	cfg2 := &TeamsConfig{ActiveTeam: "data", Teams: map[string]TeamConfig{"data": {}}}
	applyTeamOverlay("task-x", cfg2)
	if cfg2.Teams["data"].Planner.SystemPrompt == "refined" {
		t.Fatal("overlay applied to the wrong team")
	}
}
