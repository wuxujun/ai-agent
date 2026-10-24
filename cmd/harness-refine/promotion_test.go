package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/multiagenteval"
)

func jointFixture(t *testing.T) (improvement.Lesson, harness.Snapshot, ProbeReport, []multiagenteval.Case) {
	t.Helper()
	base := testBaseSnapshot(t)
	base, err := harness.NewSnapshot(base.Teams, base.SkillDigests, "test")
	if err != nil {
		t.Fatal(err)
	}
	edits := []harness.Edit{{Target: "teams.software.planner.model", Op: "set", Before: "m1", After: "m2"}, {Target: "teams.software.writer.model", Op: "set", After: "m3"}}
	refined, err := harness.Apply(base, harness.EditSet{Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	s := improvement.NewStore(t.TempDir())
	lesson, err := s.Record(t.Context(), improvement.Lesson{BaselineActiveTeam: base.Teams.ActiveTeam, Team: "software", HarnessBefore: base.ID, HarnessAfter: refined.ID, Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.001
	b := multiagenteval.VariantResult{Runtime: lesson.Team, TaskID: "base", Status: "failed", LatencyMS: 100, LLMCalls: 1, EstimatedCostUSD: &cost}
	c := multiagenteval.VariantResult{Runtime: lesson.Team + "-joint-" + lesson.ID[:12], TaskID: "candidate", Status: "completed", LatencyMS: 100, LLMCalls: 1, EstimatedCostUSD: &cost, FinalAnswer: "done", Pass: true}
	cases := []multiagenteval.Case{{Name: "joint", ExpectedStatuses: []string{"completed"}, ExpectedContains: []string{"done"}}}
	report := ProbeReport{BaseTeam: lesson.Team, Candidates: []CandidateProbe{{Index: 0, Team: lesson.Team + "-joint-" + lesson.ID[:12], Kind: "joint", EditSet: harness.EditSet{SnapshotID: base.ID, Author: "joint:" + lesson.ID, Edits: edits}, Cases: []PairedCaseResult{{Case: "joint", Base: b, Candidate: c}}}}}
	return lesson, base, report, cases
}

func TestVerifyJointProbeRejectsIncompleteOrRegressingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ProbeReport)
	}{
		{"partial edits", func(r *ProbeReport) { r.Candidates[0].EditSet.Edits = r.Candidates[0].EditSet.Edits[:1] }},
		{"wrong baseline", func(r *ProbeReport) { r.Candidates[0].EditSet.SnapshotID = "other" }},
		{"missing case", func(r *ProbeReport) { r.Candidates[0].Cases = nil }},
		{"reused task", func(r *ProbeReport) { r.Candidates[0].Cases[0].Candidate.TaskID = "base" }},
		{"false pass flag", func(r *ProbeReport) { r.Candidates[0].Cases[0].Candidate.FinalAnswer = "wrong" }},
		{"latency regression", func(r *ProbeReport) { r.Candidates[0].Cases[0].Candidate.LatencyMS = 200 }},
		{"missing price", func(r *ProbeReport) { r.Candidates[0].Cases[0].Candidate.EstimatedCostUSD = nil }},
		{"unpriced calls", func(r *ProbeReport) { v := 0.0; r.Candidates[0].Cases[0].Candidate.EstimatedCostUSD = &v }},
		{"cost regression", func(r *ProbeReport) { v := 0.002; r.Candidates[0].Cases[0].Candidate.EstimatedCostUSD = &v }},
		{"no improvement", func(r *ProbeReport) {
			b := r.Candidates[0].Cases[0].Candidate
			b.TaskID = "base"
			b.Runtime = r.BaseTeam
			r.Candidates[0].Cases[0].Base = b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _, r, cases := jointFixture(t)
			tc.mutate(&r)
			if err := verifyLessonProbe(l, r, cases, 1, 1.2, 20, 1.2, .1); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	l, _, r, cases := jointFixture(t)
	if err := verifyLessonProbe(l, r, cases, 1, 1.2, 20, 1.2, .1); err != nil {
		t.Fatal(err)
	}
	if err := verifyLessonProbe(l, r, cases, 2, 1.2, 20, 1.2, .1); err == nil {
		t.Fatal("missing repetition accepted")
	}
}

func TestLessonPromotionCLI(t *testing.T) {
	lesson, base, report, cases := jointFixture(t)
	root := t.TempDir()
	store := improvement.NewStore(filepath.Join(root, "store"))
	recorded, err := store.Record(t.Context(), lesson)
	if err != nil || recorded.ID != lesson.ID {
		t.Fatalf("record: %v", err)
	}
	basePath := filepath.Join(root, "base.json")
	writeJSON(t, basePath, base)
	common := []string{"--lessons-dir", filepath.Join(root, "store"), "--id", lesson.ID}
	args := append([]string{"prepare"}, common...)
	args = append(args, "--base-snapshot", basePath, "--out", filepath.Join(root, "joint"))
	if code := runLessons(args, io.Discard, io.Discard); code != 0 {
		t.Fatalf("prepare exit %d", code)
	}
	var manifest proposeManifest
	data, err := os.ReadFile(filepath.Join(root, "joint", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Candidates) != 1 || manifest.Candidates[0].Kind != "joint" {
		t.Fatalf("not a single joint candidate: %v", err)
	}
	reportPath := filepath.Join(root, "probe-results.json")
	datasetPath := filepath.Join(root, "cases.yaml")
	if err := os.WriteFile(datasetPath, []byte("version: 1\nthresholds:\n  max_p95_latency_ratio: 2\ncases:\n  - name: joint\n    goal: test\n    workspace: .\n    max_steps: 1\n    tool_budget: 1\n    token_budget: 10\n    llm_call_budget: 1\n    expected_statuses: [completed]\n    expected_contains: [done]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := multiagenteval.Load(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = cases
	report.DatasetSHA256 = datasetDigest(ds)
	writeJSON(t, reportPath, report)
	args = append([]string{"verify"}, common...)
	args = append(args, "--base-snapshot", basePath, "--probe-results", reportPath, "--input", datasetPath, "--repetitions", "1")
	var stderr bytes.Buffer
	if code := runLessons(args, io.Discard, &stderr); code != 0 {
		t.Fatalf("verify exit %d: %s", code, &stderr)
	}
	if _, ok, _ := store.GetPublished(t.Context(), lesson.ID); ok {
		t.Fatal("verify auto-published")
	}
	args = append([]string{"publish"}, common...)
	args = append(args, "--reason", "reviewed joint evidence")
	if code := runLessons(args, io.Discard, &stderr); code != 0 {
		t.Fatalf("publish exit %d: %s", code, &stderr)
	}
	if _, ok, err := store.GetPublished(t.Context(), lesson.ID); err != nil || !ok {
		t.Fatalf("published lesson unavailable: %v", err)
	}
	// Changing the dataset with the same case name invalidates the evidence.
	data, err = os.ReadFile(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(datasetPath, []byte(strings.Replace(string(data), "goal: test", "goal: changed", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"verify"}, common...)
	args = append(args, "--base-snapshot", basePath, "--probe-results", reportPath, "--input", datasetPath, "--repetitions", "1")
	if code := runLessons(args, io.Discard, &stderr); code != 1 {
		t.Fatal("changed dataset accepted")
	}
}

func TestLessonSnapshotRejectsUnprobedTeamEdits(t *testing.T) {
	l, base, _, _ := jointFixture(t)
	base.Teams.Teams["data"] = base.Teams.Teams["software"]
	base, err := harness.NewSnapshot(base.Teams, base.SkillDigests, "cross-team")
	if err != nil {
		t.Fatal(err)
	}
	l.Edits = []harness.Edit{{Target: "teams.data.planner.model", Op: "set", After: "other-model"}}
	refined, err := harness.Apply(base, harness.EditSet{Edits: l.Edits})
	if err != nil {
		t.Fatal(err)
	}
	l.HarnessBefore = base.ID
	l.HarnessAfter = refined.ID
	if err := checkLessonSnapshot(l, base); err == nil {
		t.Fatal("joint probe would not materialize the edited team")
	}
}
