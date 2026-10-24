package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/multiagenteval"
	"gopkg.in/yaml.v3"
)

func runLessonPromotion(args []string, stdout, stderr io.Writer) int {
	action := args[0]
	fs := flag.NewFlagSet("lessons "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("lessons-dir", "", "lesson store (required)")
	id := fs.String("id", "", "full lesson ID (required)")
	basePath := fs.String("base-snapshot", "", "original base snapshot (prepare/verify)")
	out := fs.String("out", "", "new candidate directory (prepare)")
	reportPath := fs.String("probe-results", "", "joint probe report (verify)")
	input := fs.String("input", "", "reviewed evaluation dataset (verify)")
	repetitions := fs.Int("repetitions", 2, "required repetitions of every dataset case (verify)")
	costRatio := fs.Float64("max-cost-ratio", 1.2, "maximum candidate/base estimated USD cost per pair")
	maxCost := fs.Float64("max-task-cost-usd", 0.1, "maximum estimated USD cost per candidate task")
	ratio := fs.Float64("max-latency-ratio", 1.2, "maximum candidate/base latency per pair (verify)")
	calls := fs.Int("max-llm-calls", 20, "maximum LLM calls per candidate task (verify; not a USD budget)")
	reason := fs.String("reason", "", "operator publication reason (publish)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *dir == "" || *id == "" {
		fmt.Fprintln(stderr, "--lessons-dir and --id are required")
		return 2
	}
	ctx := context.Background()
	store := improvement.NewStore(*dir)
	if action == "publish" {
		if err := store.Publish(ctx, *id, *reason); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "published lesson %s\n", *id)
		return 0
	}
	lesson, ok, err := store.Get(ctx, *id)
	if err != nil || !ok {
		fmt.Fprintf(stderr, "lesson unavailable: %v\n", err)
		return 1
	}
	var base harness.Snapshot
	data, err := os.ReadFile(*basePath)
	if err == nil {
		err = json.Unmarshal(data, &base)
	}
	if err == nil {
		err = checkLessonSnapshot(lesson, base)
	}
	if err != nil {
		fmt.Fprintf(stderr, "base snapshot: %v\n", err)
		return 1
	}
	if action == "prepare" {
		if *out == "" {
			fmt.Fprintln(stderr, "--out is required")
			return 2
		}
		if err := prepareLessonProbe(lesson, base, *out); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "joint candidate prepared at %s; load teams.candidates.yaml on an isolated service, then probe\n", *out)
		return 0
	}
	if *input == "" || *reportPath == "" || *repetitions < 1 || math.IsNaN(*ratio) || *ratio <= 0 || *ratio > 10 || *calls <= 0 || !positiveBounded(*costRatio, 10) || !positiveBounded(*maxCost, 1000) {
		fmt.Fprintln(stderr, "verify requires --input, --probe-results and positive bounded thresholds")
		return 2
	}
	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ds, err := multiagenteval.Load(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	data, err = os.ReadFile(*reportPath)
	var report ProbeReport
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	if err == nil && report.DatasetSHA256 != datasetDigest(ds) {
		err = fmt.Errorf("evaluation dataset digest mismatch")
	}
	if err == nil {
		err = verifyLessonProbe(lesson, report, ds.Cases, *repetitions, *ratio, *calls, *costRatio, *maxCost)
	}
	if err != nil {
		fmt.Fprintf(stderr, "joint validation failed: %v\n", err)
		return 1
	}
	sum := sha256.Sum256(data)
	err = store.Validate(ctx, improvement.Validation{LessonID: lesson.ID, ReportSHA256: hex.EncodeToString(sum[:]), DatasetSHA256: datasetDigest(ds), MaxCostRatio: *costRatio, MaxTaskCostUSD: *maxCost, Cases: len(ds.Cases) * (*repetitions), MaxLatencyRatio: *ratio, MaxLLMCalls: *calls})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "validated lesson %s; explicit lessons publish is still required\n", lesson.ID)
	return 0
}

func checkLessonSnapshot(l improvement.Lesson, base harness.Snapshot) error {
	actual, err := harness.NewSnapshot(base.Teams, base.SkillDigests, "verify")
	if err != nil {
		return err
	}
	if actual.ID != base.ID || base.ID != l.HarnessBefore || base.Teams.ActiveTeam != l.BaselineActiveTeam {
		return fmt.Errorf("base snapshot digest mismatch")
	}
	refined, err := harness.Apply(base, harness.EditSet{SnapshotID: base.ID, Edits: l.Edits})
	if err != nil {
		return err
	}
	if refined.ID != l.HarnessAfter {
		return fmt.Errorf("refined snapshot digest mismatch")
	}
	return nil
}

func prepareLessonProbe(l improvement.Lesson, base harness.Snapshot, out string) error {
	refined, err := harness.Apply(base, harness.EditSet{SnapshotID: base.ID, Edits: l.Edits})
	if err != nil {
		return err
	}
	// Exclusive directory creation avoids mixing evidence from previous runs.
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0755); err != nil {
		return err
	}
	team := l.Team + "-joint-" + l.ID[:12]
	if _, exists := base.Teams.Teams[team]; exists {
		return fmt.Errorf("joint team already exists")
	}
	merged := base.Teams
	merged.Teams = make(map[string]multiagent.TeamConfig, len(base.Teams.Teams)+1)
	for name, cfg := range base.Teams.Teams {
		merged.Teams[name] = cfg
	}
	merged.Teams[team] = refined.Teams.Teams[l.Team]
	manifest := proposeManifest{BaseTeam: l.Team, BaseSnapshot: "base-snapshot.json", TeamsFile: "teams.candidates.yaml"}
	manifest.Candidates = append(manifest.Candidates, struct {
		Index int    `json:"index"`
		Team  string `json:"team"`
		File  string `json:"file"`
		Kind  string `json:"kind"`
	}{Index: 0, Team: team, File: "candidate-00.json", Kind: "joint"})
	for name, value := range map[string]any{"manifest.json": manifest, "base-snapshot.json": base, "candidate-00.json": harness.EditSet{SnapshotID: base.ID, Edits: l.Edits, Author: "joint:" + l.ID}} {
		if err := writeJSONFile(filepath.Join(out, name), value); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(merged)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, manifest.TeamsFile), data, 0600)
}

func verifyLessonProbe(l improvement.Lesson, report ProbeReport, cases []multiagenteval.Case, repetitions int, ratio float64, calls int, costRatio, maxCost float64) error {
	if report.BaseTeam != l.Team || len(report.Candidates) != 1 {
		return fmt.Errorf("expected exactly one joint candidate for the lesson team")
	}
	cp := report.Candidates[0]
	if cp.Team != l.Team+"-joint-"+l.ID[:12] || cp.Kind != "joint" || cp.EditSet.Author != "joint:"+l.ID || cp.EditSet.SnapshotID != l.HarnessBefore || !reflect.DeepEqual(cp.EditSet.Edits, l.Edits) {
		return fmt.Errorf("report does not evaluate the exact lesson")
	}
	if len(cases) == 0 || len(cp.Cases) != len(cases)*repetitions {
		return fmt.Errorf("incomplete evaluation matrix")
	}
	expected := map[string]bool{}
	definitions := map[string]multiagenteval.Case{}
	for _, c := range cases {
		definitions[c.Name] = c
		for rep := 0; rep < repetitions; rep++ {
			key := fmt.Sprintf("%s\x00%d", c.Name, rep)
			if _, ok := expected[key]; ok {
				return fmt.Errorf("duplicate dataset case")
			}
			expected[key] = false
		}
	}
	seenTasks := map[string]bool{}
	improved := false
	for _, pair := range cp.Cases {
		key := fmt.Sprintf("%s\x00%d", pair.Case, pair.Repetition)
		if seen, ok := expected[key]; !ok || seen {
			return fmt.Errorf("duplicate or unexpected case")
		}
		expected[key] = true
		b, c := pair.Base, pair.Candidate
		multiagenteval.EvaluateCase(definitions[pair.Case], &b)
		multiagenteval.EvaluateCase(definitions[pair.Case], &c)
		for _, id := range []string{b.TaskID, c.TaskID} {
			if id == "" || seenTasks[id] {
				return fmt.Errorf("missing or reused task identity")
			}
			seenTasks[id] = true
		}
		if b.Status == "" || b.Status == "created" || b.Status == "running" || b.LLMCalls < 0 || c.LLMCalls < 0 || b.Runtime != l.Team || c.Runtime != cp.Team || b.Error != "" || c.Error != "" || c.Status != "completed" || !c.Pass || (b.Supported && !c.Supported) || c.FailedToolCount > b.FailedToolCount {
			return fmt.Errorf("failed or regressing joint case %s", pair.Case)
		}
		if b.LatencyMS <= 0 || c.LatencyMS <= 0 || float64(c.LatencyMS) > float64(b.LatencyMS)*ratio || c.LLMCalls < 0 || c.LLMCalls > calls {
			return fmt.Errorf("latency or call budget failed for %s", pair.Case)
		}
		if b.EstimatedCostUSD == nil || c.EstimatedCostUSD == nil || !validEstimatedCost(*b.EstimatedCostUSD, b.LLMCalls) || !validEstimatedCost(*c.EstimatedCostUSD, c.LLMCalls) || *c.EstimatedCostUSD > maxCost || *c.EstimatedCostUSD > *b.EstimatedCostUSD*costRatio {
			return fmt.Errorf("missing, invalid or regressing cost evidence for %s", pair.Case)
		}
		if !b.Pass || (!b.Supported && c.Supported) {
			improved = true
		}
	}
	if !improved {
		return fmt.Errorf("joint candidate has no measured improvement")
	}
	return nil
}

func positiveBounded(v, max float64) bool { return v > 0 && v <= max }
func validEstimatedCost(v float64, calls int) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && (calls == 0 || v > 0)
}
func datasetDigest(ds multiagenteval.Dataset) string {
	data, _ := json.Marshal(ds)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
