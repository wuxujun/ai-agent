package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"gopkg.in/yaml.v3"
)

// candidateAgg holds per-candidate signals aggregated over paired cases.
// This is the no-ground-truth behavioral contrast: we compare what the two
// harnesses DID (pass, verifier support, tool failures, steps), never an
// external answer key.
type candidateAgg struct {
	index         int
	team          string
	kind          string
	cases         int
	basePass      int
	candPass      int
	baseSupported int
	candSupported int
	candFailed    int
	candSteps     int
	candLatencyMS []int64
}

func (a *candidateAgg) passRate() float64 {
	if a.cases == 0 {
		return 0
	}
	return float64(a.candPass) / float64(a.cases)
}

func (a *candidateAgg) basePassRate() float64 {
	if a.cases == 0 {
		return 0
	}
	return float64(a.basePass) / float64(a.cases)
}

func (a *candidateAgg) supportedRate() float64 {
	if a.cases == 0 {
		return 0
	}
	return float64(a.candSupported) / float64(a.cases)
}

func (a *candidateAgg) baseSupportedRate() float64 {
	if a.cases == 0 {
		return 0
	}
	return float64(a.baseSupported) / float64(a.cases)
}

// betterThan implements the lexicographic ranking also used for adoption:
// pass rate, then verifier-supported rate, then fewer failed tools,
// then fewer steps. Mirrors Harness^2's contrastive compose: keep edits
// supported by the execution evidence.
func (a *candidateAgg) betterThan(b *candidateAgg) bool {
	if a.passRate() != b.passRate() {
		return a.passRate() > b.passRate()
	}
	if a.supportedRate() != b.supportedRate() {
		return a.supportedRate() > b.supportedRate()
	}
	if a.candFailed != b.candFailed {
		return a.candFailed < b.candFailed
	}
	return a.candSteps < b.candSteps
}

// betterThanBase reports whether the candidate strictly improves on its own
// paired base runs.
func (a *candidateAgg) betterThanBase() bool {
	if a.passRate() != a.basePassRate() {
		return a.passRate() > a.basePassRate()
	}
	if a.supportedRate() != a.baseSupportedRate() {
		return a.supportedRate() > a.baseSupportedRate()
	}
	// Same pass/support: require strictly fewer failures to adopt.
	// (Base failure/step counts are not tracked per candidate; the bar is
	// deliberately conservative: no evidence of harm is not adoption.)
	return false
}

// clamp01 bounds v to [0,1].
func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// mergeResult is the outcome of merging the edit sets of all candidates
// that strictly beat the base.
type mergeResult struct {
	editSet harness.EditSet
	// from lists contributing candidate indices in rank order (best first).
	from []int
	// dropped counts edits skipped because a higher-ranked candidate
	// claimed the same target with a different change.
	dropped int
	// fellBack is true when the merged set failed validation and compose
	// fell back to the winner's edit set alone.
	fellBack bool
}

// mergeEditSets combines the edit sets of every candidate that strictly beat
// the base, in rank order. A target claimed by a higher-ranked candidate wins
// on conflict; identical edits dedupe. The merged combination was never
// probed jointly — the next refinement round probes it as its base.
func mergeEditSets(base harness.Snapshot, aggs []*candidateAgg, candidates []CandidateProbe, winnerIdx int) mergeResult {
	byIndex := map[int]harness.EditSet{}
	for _, cp := range candidates {
		byIndex[cp.Index] = cp.EditSet
	}
	claimed := map[string]harness.Edit{}
	var merged []harness.Edit
	var from []int
	dropped := 0
	for _, a := range aggs {
		if !a.betterThanBase() {
			continue
		}
		contributed := false
		for _, e := range byIndex[a.index].Edits {
			if prev, ok := claimed[e.Target]; ok {
				if prev.Op == e.Op && prev.After == e.After {
					continue // identical edit, dedupe
				}
				dropped++ // conflict: higher-ranked candidate wins
				continue
			}
			claimed[e.Target] = e
			merged = append(merged, e)
			contributed = true
		}
		if contributed {
			from = append(from, a.index)
		}
	}
	res := mergeResult{
		editSet: harness.EditSet{SnapshotID: base.ID, Edits: merged, Author: "compose:merge"},
		from:    from,
		dropped: dropped,
	}
	if err := harness.Validate(res.editSet); err != nil {
		res.editSet = byIndex[winnerIdx]
		res.from = []int{winnerIdx}
		res.dropped = 0
		res.fellBack = true
	}
	return res
}

type composeOutput struct {
	Adopted          bool      `json:"adopted"`
	WinnerIndex      int       `json:"winner_index"`
	WinnerTeam       string    `json:"winner_team"`
	Reason           string    `json:"reason"`
	MergedFrom       []int     `json:"merged_from,omitempty"`
	ConflictsDropped int       `json:"conflicts_dropped,omitempty"`
	MergeFallback    bool      `json:"merge_fallback,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

func aggregate(report ProbeReport) []*candidateAgg {
	var aggs []*candidateAgg
	for _, cp := range report.Candidates {
		a := &candidateAgg{index: cp.Index, team: cp.Team, kind: cp.Kind}
		for _, c := range cp.Cases {
			a.cases++
			if c.Base.Pass {
				a.basePass++
			}
			if c.Candidate.Pass {
				a.candPass++
			}
			if c.Base.Supported {
				a.baseSupported++
			}
			if c.Candidate.Supported {
				a.candSupported++
			}
			a.candFailed += c.Candidate.FailedToolCount
			a.candSteps += c.Candidate.StepCount
			a.candLatencyMS = append(a.candLatencyMS, c.Candidate.LatencyMS)
		}
		aggs = append(aggs, a)
	}
	sort.Slice(aggs, func(i, j int) bool { return aggs[i].betterThan(aggs[j]) })
	return aggs
}

func runCompose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	probeResults := fs.String("probe-results", "", "probe-results.json from the probe phase (required)")
	baseSnapshot := fs.String("base-snapshot", "", "base-snapshot.json from the propose phase (required)")
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory: record the adopted lesson here")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *probeResults == "" || *baseSnapshot == "" || *out == "" {
		fmt.Fprintln(stderr, "compose: --probe-results, --base-snapshot and --out are required")
		return 2
	}

	prData, err := os.ReadFile(*probeResults)
	if err != nil {
		fmt.Fprintf(stderr, "compose: read probe results: %v\n", err)
		return 1
	}
	var report ProbeReport
	if err := json.Unmarshal(prData, &report); err != nil {
		fmt.Fprintf(stderr, "compose: parse probe results: %v\n", err)
		return 1
	}
	if len(report.Candidates) == 0 {
		fmt.Fprintln(stderr, "compose: no candidates in probe results")
		return 1
	}
	bsData, err := os.ReadFile(*baseSnapshot)
	if err != nil {
		fmt.Fprintf(stderr, "compose: read base snapshot: %v\n", err)
		return 1
	}
	var base harness.Snapshot
	if err := json.Unmarshal(bsData, &base); err != nil {
		fmt.Fprintf(stderr, "compose: parse base snapshot: %v\n", err)
		return 1
	}

	aggs := aggregate(report)
	winner := aggs[0]
	adopted := winner.betterThanBase()

	output := composeOutput{
		WinnerIndex: winner.index,
		WinnerTeam:  winner.team,
		CreatedAt:   time.Now().UTC(),
		Adopted:     adopted,
	}
	var mr mergeResult
	if adopted {
		output.Reason = fmt.Sprintf("candidate %d (%s) strictly improves on base: pass %.2f -> %.2f, supported %.2f -> %.2f",
			winner.index, winner.kind, winner.basePassRate(), winner.passRate(),
			winner.baseSupportedRate(), winner.supportedRate())
		// Merge the edit sets of all candidates that beat the base
		// (conflicts resolve to the higher-ranked candidate).
		mr = mergeEditSets(base, aggs, report.Candidates, winner.index)
		output.MergedFrom = mr.from
		output.ConflictsDropped = mr.dropped
		output.MergeFallback = mr.fellBack
	} else {
		output.Reason = fmt.Sprintf("no candidate strictly beats the base; best was %d (%s) with pass %.2f vs base %.2f",
			winner.index, winner.kind, winner.passRate(), winner.basePassRate())
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(stderr, "compose: mkdir: %v\n", err)
		return 1
	}
	writeJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(*out, name), append(data, '\n'), 0o644)
	}
	if err := writeJSON("compose.json", output); err != nil {
		fmt.Fprintf(stderr, "compose: %v\n", err)
		return 1
	}

	var md strings.Builder
	md.WriteString("# Harness refinement report\n\n")
	md.WriteString(fmt.Sprintf("Base team `%s`, service `%s`, probed at %s.\n\n",
		report.BaseTeam, report.ServiceURL, report.CreatedAt.Format(time.RFC3339)))
	md.WriteString("| # | kind | team | cases | base pass | cand pass | base supported | cand supported | cand failed tools | cand steps |\n")
	md.WriteString("|---|------|------|-------|-----------|-----------|----------------|----------------|-------------------|------------|\n")
	for _, a := range aggs {
		mark := ""
		if adopted && a.index == winner.index {
			mark = " ✅ adopted"
		}
		fmt.Fprintf(&md, "| %d | %s | `%s` | %d | %.2f | %.2f | %.2f | %.2f | %d | %d |%s\n",
			a.index, a.kind, a.team, a.cases,
			a.basePassRate(), a.passRate(), a.baseSupportedRate(), a.supportedRate(),
			a.candFailed, a.candSteps, mark)
	}
	md.WriteString("\n" + output.Reason + "\n")
	if adopted {
		fmt.Fprintf(&md, "\nAdopted edits were merged from candidate(s) %v", mr.from)
		if mr.dropped > 0 {
			fmt.Fprintf(&md, " (%d conflicting edit(s) dropped in favor of higher-ranked candidates)", mr.dropped)
		}
		if mr.fellBack {
			md.WriteString(" — the merged set failed validation, fell back to the winner's edits alone")
		}
		md.WriteString(".\n\nNote: the merged combination was not probed jointly; the next refinement round probes it as its base.\n")
	}
	if err := os.WriteFile(filepath.Join(*out, "report.md"), []byte(md.String()), 0o644); err != nil {
		fmt.Fprintf(stderr, "compose: %v\n", err)
		return 1
	}

	if adopted {
		adoptedES := mr.editSet
		if err := writeJSON("refined-edits.json", adoptedES); err != nil {
			fmt.Fprintf(stderr, "compose: %v\n", err)
			return 1
		}
		refined, err := harness.Apply(base, adoptedES)
		if err != nil {
			fmt.Fprintf(stderr, "compose: apply adopted edits: %v\n", err)
			return 1
		}
		if err := writeJSON("refined-snapshot.json", refined); err != nil {
			fmt.Fprintf(stderr, "compose: %v\n", err)
			return 1
		}
		// Materialize the refined teams config as YAML: this is the next
		// iteration round's --teams-yaml input.
		teamsYAML, err := yaml.Marshal(refined.Teams)
		if err != nil {
			fmt.Fprintf(stderr, "compose: marshal refined teams: %v\n", err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(*out, "teams.refined.yaml"), teamsYAML, 0o644); err != nil {
			fmt.Fprintf(stderr, "compose: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "adopted %d edit(s) merged from candidate(s) %v (winner %d %s): refined snapshot %s\n",
			len(adoptedES.Edits), mr.from, winner.index, winner.team, refined.ID[:12])
		if mr.fellBack {
			fmt.Fprintf(stdout, "merged set failed validation, fell back to winner's edits alone\n")
		}
		fmt.Fprintf(stdout, "review report.md, then publish refined-edits.json through human review\n")
		if *lessonsDir != "" {
			evidence := make([]improvement.EvidenceRef, 0, len(mr.from))
			for _, idx := range mr.from {
				evidence = append(evidence, improvement.EvidenceRef{
					Kind:    "probe-contrast",
					Summary: output.Reason,
					URI:     fmt.Sprintf("probe-results.json#candidate-%d", idx),
				})
			}
			lesson := improvement.Lesson{
				Team:          report.BaseTeam,
				HarnessBefore: base.ID,
				HarnessAfter:  refined.ID,
				Edits:         adoptedES.Edits,
				Evidence:      evidence,
				ContrastDelta: fmt.Sprintf("pass %.2f -> %.2f, supported %.2f -> %.2f",
					winner.basePassRate(), winner.passRate(),
					winner.baseSupportedRate(), winner.supportedRate()),
				Confidence: clamp01(math.Max(
					winner.passRate()-winner.basePassRate(),
					winner.supportedRate()-winner.baseSupportedRate(),
				)),
				Source: "harness-refine:compose",
			}
			rec, err := improvement.NewStore(*lessonsDir).Record(context.Background(), lesson)
			if err != nil {
				fmt.Fprintf(stderr, "compose: record lesson: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "recorded improvement lesson %s\n", rec.ID[:12])
		}
		return 0
	}
	fmt.Fprintf(stdout, "no candidate adopted: %s\n", output.Reason)
	return exitNoAdoption
}
