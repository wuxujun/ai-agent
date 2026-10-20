package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/multiagenteval"
)

func vr(pass, supported bool, failedTools, steps int) multiagenteval.VariantResult {
	return multiagenteval.VariantResult{
		Status: "completed", Pass: pass, Supported: supported,
		FailedToolCount: failedTools, StepCount: steps, LatencyMS: 100,
	}
}

func testReport() ProbeReport {
	mk := func(idx int, team string, pairs ...[2]multiagenteval.VariantResult) CandidateProbe {
		cp := CandidateProbe{Index: idx, Team: team, EditSet: harness.EditSet{Author: "test"}}
		for i, p := range pairs {
			cp.Cases = append(cp.Cases, PairedCaseResult{
				Case: "c", Repetition: i, Base: p[0], Candidate: p[1],
			})
		}
		return cp
	}
	return ProbeReport{
		BaseTeam: "software", ServiceURL: "http://x", CreatedAt: time.Now().UTC(),
		Candidates: []CandidateProbe{
			// candidate 0: strictly better than base
			withEdits(mk(0, "software-cand-00",
				[2]multiagenteval.VariantResult{vr(true, true, 1, 8), vr(true, true, 0, 6)},
				[2]multiagenteval.VariantResult{vr(false, false, 2, 9), vr(true, true, 0, 5)},
			), harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "test"}),
			// candidate 1: worse than base
			mk(1, "software-cand-01",
				[2]multiagenteval.VariantResult{vr(true, true, 0, 5), vr(false, false, 3, 9)},
				[2]multiagenteval.VariantResult{vr(true, true, 0, 5), vr(true, false, 1, 7)},
			),
		},
	}
}

func withEdits(cp CandidateProbe, edits ...harness.Edit) CandidateProbe {
	cp.EditSet.Edits = edits
	return cp
}

func TestAggregateRanking(t *testing.T) {
	aggs := aggregate(testReport())
	if len(aggs) != 2 {
		t.Fatalf("want 2 aggregates, got %d", len(aggs))
	}
	if aggs[0].index != 0 {
		t.Fatalf("winner should be candidate 0, got %d", aggs[0].index)
	}
	if !aggs[0].betterThanBase() {
		t.Fatal("candidate 0 should beat its base")
	}
	if aggs[1].betterThanBase() {
		t.Fatal("candidate 1 should not beat its base")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testBaseSnapshot(t *testing.T) harness.Snapshot {
	t.Helper()
	return harness.Snapshot{
		ID:     "base-id",
		Source: "test",
		Teams: multiagent.TeamsConfig{
			ActiveTeam: "software",
			Teams: map[string]multiagent.TeamConfig{
				"software": {
					Planner: multiagent.AgentConfig{Model: "m1"},
				},
			},
		},
	}
}

func TestComposeAdopts(t *testing.T) {
	out := t.TempDir()
	prPath := filepath.Join(out, "probe-results.json")
	bsPath := filepath.Join(out, "base-snapshot.json")
	writeJSON(t, prPath, testReport())
	writeJSON(t, bsPath, testBaseSnapshot(t))

	code := runCompose([]string{
		"--probe-results", prPath, "--base-snapshot", bsPath, "--out", out,
	}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("runCompose exit %d, want 0 (adopted)", code)
	}
	for _, name := range []string{"compose.json", "report.md", "refined-edits.json", "refined-snapshot.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("missing %s", name)
		}
	}
	data, _ := os.ReadFile(filepath.Join(out, "compose.json"))
	var co composeOutput
	if err := json.Unmarshal(data, &co); err != nil {
		t.Fatal(err)
	}
	if !co.Adopted || co.WinnerIndex != 0 {
		t.Fatalf("bad compose output: %+v", co)
	}
}

func TestComposeNoAdoption(t *testing.T) {
	report := testReport()
	// Make every candidate worse than base.
	for ci := range report.Candidates {
		for i := range report.Candidates[ci].Cases {
			c := &report.Candidates[ci].Cases[i]
			c.Base = vr(true, true, 0, 4)
			c.Candidate = vr(false, false, 5, 12)
		}
	}
	out := t.TempDir()
	prPath := filepath.Join(out, "probe-results.json")
	bsPath := filepath.Join(out, "base-snapshot.json")
	writeJSON(t, prPath, report)
	writeJSON(t, bsPath, testBaseSnapshot(t))

	code := runCompose([]string{
		"--probe-results", prPath, "--base-snapshot", bsPath, "--out", out,
	}, io.Discard, io.Discard)
	if code != exitNoAdoption {
		t.Fatalf("runCompose exit %d, want %d (no adoption)", code, exitNoAdoption)
	}
	if _, err := os.Stat(filepath.Join(out, "refined-snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("refined-snapshot.json should not exist when nothing is adopted")
	}
}

func beatingCandidate(idx int, team string, edits ...harness.Edit) CandidateProbe {
	mk := func(pairs ...[2]multiagenteval.VariantResult) CandidateProbe {
		cp := CandidateProbe{Index: idx, Team: team, EditSet: harness.EditSet{Author: "test"}}
		for i, p := range pairs {
			cp.Cases = append(cp.Cases, PairedCaseResult{
				Case: "c", Repetition: i, Base: p[0], Candidate: p[1],
			})
		}
		return withEdits(cp, edits...)
	}
	return mk(
		[2]multiagenteval.VariantResult{vr(true, true, 1, 8), vr(true, true, 0, 6)},
		[2]multiagenteval.VariantResult{vr(false, false, 2, 9), vr(true, true, 0, 5)},
	)
}

func TestMergeNonConflicting(t *testing.T) {
	base := testBaseSnapshot(t)
	report := ProbeReport{
		BaseTeam: "software",
		Candidates: []CandidateProbe{
			beatingCandidate(0, "software-cand-00",
				harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "t"}),
			beatingCandidate(1, "software-cand-01",
				harness.Edit{Target: "teams.software.planner.provider", Op: "set", After: "p2", Reason: "t"}),
		},
	}
	aggs := aggregate(report)
	mr := mergeEditSets(base, aggs, report.Candidates, aggs[0].index)
	if mr.fellBack {
		t.Fatal("valid merge should not fall back")
	}
	if len(mr.editSet.Edits) != 2 {
		t.Fatalf("want 2 merged edits, got %d", len(mr.editSet.Edits))
	}
	if len(mr.from) != 2 || mr.from[0] != 0 || mr.from[1] != 1 {
		t.Fatalf("want from=[0 1], got %v", mr.from)
	}
	if mr.dropped != 0 {
		t.Fatalf("want 0 dropped, got %d", mr.dropped)
	}
}

func TestMergeConflictResolvesToHigherRank(t *testing.T) {
	base := testBaseSnapshot(t)
	report := ProbeReport{
		BaseTeam: "software",
		Candidates: []CandidateProbe{
			beatingCandidate(0, "software-cand-00",
				harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "t"}),
			beatingCandidate(1, "software-cand-01",
				harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m3", Reason: "t"}),
		},
	}
	aggs := aggregate(report)
	mr := mergeEditSets(base, aggs, report.Candidates, aggs[0].index)
	if len(mr.editSet.Edits) != 1 {
		t.Fatalf("want 1 merged edit, got %d", len(mr.editSet.Edits))
	}
	if mr.editSet.Edits[0].After != "m2" {
		t.Fatalf("conflict should resolve to higher-ranked candidate, got %q", mr.editSet.Edits[0].After)
	}
	if mr.dropped != 1 {
		t.Fatalf("want 1 dropped, got %d", mr.dropped)
	}
}

func TestMergeIdenticalDedupes(t *testing.T) {
	base := testBaseSnapshot(t)
	e := harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "t"}
	report := ProbeReport{
		BaseTeam: "software",
		Candidates: []CandidateProbe{
			beatingCandidate(0, "software-cand-00", e),
			beatingCandidate(1, "software-cand-01", e),
		},
	}
	aggs := aggregate(report)
	mr := mergeEditSets(base, aggs, report.Candidates, aggs[0].index)
	if len(mr.editSet.Edits) != 1 {
		t.Fatalf("identical edits should dedupe to 1, got %d", len(mr.editSet.Edits))
	}
	if mr.dropped != 0 {
		t.Fatalf("dedupe is not a conflict: want 0 dropped, got %d", mr.dropped)
	}
}

func TestMergeSkipsNonBeating(t *testing.T) {
	base := testBaseSnapshot(t)
	report := testReport() // candidate 1 does not beat base
	// Give the non-beating candidate a non-conflicting edit anyway.
	report.Candidates[1] = withEdits(report.Candidates[1],
		harness.Edit{Target: "teams.software.planner.provider", Op: "set", After: "p9", Reason: "t"})
	aggs := aggregate(report)
	mr := mergeEditSets(base, aggs, report.Candidates, aggs[0].index)
	for _, e := range mr.editSet.Edits {
		if e.After == "p9" {
			t.Fatal("edits from non-beating candidates must not be merged")
		}
	}
	if len(mr.from) != 1 || mr.from[0] != 0 {
		t.Fatalf("want from=[0], got %v", mr.from)
	}
}

func TestComposeMergesFromMultipleCandidates(t *testing.T) {
	out := t.TempDir()
	base := testBaseSnapshot(t)
	report := ProbeReport{
		BaseTeam: "software", ServiceURL: "http://x", CreatedAt: time.Now().UTC(),
		Candidates: []CandidateProbe{
			beatingCandidate(0, "software-cand-00",
				harness.Edit{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "t"}),
			beatingCandidate(1, "software-cand-01",
				harness.Edit{Target: "teams.software.planner.provider", Op: "set", After: "p2", Reason: "t"}),
		},
	}
	prPath := filepath.Join(out, "probe-results.json")
	bsPath := filepath.Join(out, "base-snapshot.json")
	writeJSON(t, prPath, report)
	writeJSON(t, bsPath, base)

	if code := runCompose([]string{
		"--probe-results", prPath, "--base-snapshot", bsPath, "--out", out,
	}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("runCompose exit %d, want 0", code)
	}
	var adopted struct {
		Edits []harness.Edit `json:"edits"`
	}
	data, err := os.ReadFile(filepath.Join(out, "refined-edits.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &adopted); err != nil {
		t.Fatal(err)
	}
	if len(adopted.Edits) != 2 {
		t.Fatalf("want 2 merged edits in refined-edits.json, got %d", len(adopted.Edits))
	}
}
