package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/refine"
	"gopkg.in/yaml.v3"
)

// proposeManifest records what propose produced so probe and compose can
// consume it without re-deriving names.
type proposeManifest struct {
	BaseTeam     string `json:"base_team"`
	BaseSnapshot string `json:"base_snapshot"` // file name
	Candidates   []struct {
		Index int    `json:"index"`
		Team  string `json:"team"`
		File  string `json:"file"`
		Kind  string `json:"kind"` // "general" | "domain"
	} `json:"candidates"`
	TeamsFile string `json:"teams_file"` // materialized teams.yaml with candidate teams
}

func candidateTeamName(team string, i int) string {
	return fmt.Sprintf("%s-cand-%02d", team, i)
}

func runPropose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	teamsYAML := fs.String("teams-yaml", "teams.yaml", "path to teams.yaml")
	skillsRoot := fs.String("skills-root", "skills", "skills directory (digested into the snapshot)")
	team := fs.String("team", "", "base team to refine (required)")
	k := fs.Int("k", 3, "candidates per scope (general and domain)")
	editorName := fs.String("editor", "mock", "editor: mock | llm")
	editorMaxCostUSD := fs.Float64("editor-max-cost-usd", 0, "ceiling for the llm editor call cost in USD (0 = no ceiling)")
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory: surface lessons as experience, skip already-adopted edits")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *team == "" || *out == "" {
		fmt.Fprintln(stderr, "propose: --team and --out are required")
		return 2
	}

	base, err := harness.Capture(*teamsYAML, *skillsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "propose: capture base harness: %v\n", err)
		return 1
	}

	var editor refine.Editor
	switch *editorName {
	case "mock":
		editor = refine.MockEditor{}
	case "llm":
		editor = refine.LLMEditor{MaxCostUSD: *editorMaxCostUSD}
	default:
		fmt.Fprintf(stderr, "propose: unknown editor %q\n", *editorName)
		return 2
	}

	// Harnessing-level recursion: load the improvement harness first so the
	// editor can use adopted lessons as experience, then skip candidates
	// whose edits were already adopted.
	var lessons []improvement.Lesson
	if *lessonsDir != "" {
		var err error
		lessons, err = improvement.NewStore(*lessonsDir).List(context.Background(), *team, 20)
		if err != nil {
			fmt.Fprintf(stderr, "propose: load lessons: %v\n", err)
			return 1
		}
		if len(lessons) > 0 {
			fmt.Fprintf(stdout, "experience: %d lesson(s) for team %q\n", len(lessons), *team)
			for _, l := range lessons {
				fmt.Fprintf(stdout, "  - %s: %s (confidence %.2f)\n", l.ID[:12], l.ContrastDelta, l.Confidence)
			}
		}
	}

	general, domain, usage, err := editor.Propose(context.Background(), base, *team, *k, lessons)
	if err != nil {
		fmt.Fprintf(stderr, "propose: %v\n", err)
		return 1
	}
	if usage.TotalTokens > 0 {
		fmt.Fprintf(stdout, "editor usage: %d prompt + %d completion tokens\n",
			usage.PromptTokens, usage.CompletionTokens)
		if le, ok := editor.(refine.LLMEditor); ok {
			fmt.Fprintf(stdout, "editor estimated cost: $%.4f\n", le.EstimateCost(usage))
		}
	}
	candidates := append(append([]harness.EditSet{}, general...), domain...)
	kinds := append(append([]string{}, repeat("general", len(general))...), repeat("domain", len(domain))...)

	if *lessonsDir != "" {
		var dropped int
		candidates, kinds, dropped = filterAdopted(candidates, kinds, lessons)
		if dropped > 0 {
			fmt.Fprintf(stdout, "skipped %d candidate(s) already adopted by the improvement harness\n", dropped)
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(stderr, "propose: mkdir: %v\n", err)
		return 1
	}
	writeJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(*out, name), append(data, '\n'), 0o644)
	}
	if err := writeJSON("base-snapshot.json", base); err != nil {
		fmt.Fprintf(stderr, "propose: %v\n", err)
		return 1
	}

	manifest := proposeManifest{BaseTeam: *team, BaseSnapshot: "base-snapshot.json", TeamsFile: "teams.candidates.yaml"}
	// Deep-copy the team map: candidate teams are added without mutating
	// the base snapshot.
	merged := base.Teams
	mergedTeams := make(map[string]multiagent.TeamConfig, len(base.Teams.Teams)+len(candidates))
	for name, cfg := range base.Teams.Teams {
		mergedTeams[name] = cfg
	}
	for i, es := range candidates {
		name := fmt.Sprintf("candidate-%02d.json", i)
		if err := writeJSON(name, es); err != nil {
			fmt.Fprintf(stderr, "propose: %v\n", err)
			return 1
		}
		candSnap, err := harness.Apply(base, es)
		if err != nil {
			fmt.Fprintf(stderr, "propose: apply candidate %d: %v\n", i, err)
			return 1
		}
		candTeamName := candidateTeamName(*team, i)
		mergedTeams[candTeamName] = candSnap.Teams.Teams[*team]
		manifest.Candidates = append(manifest.Candidates, struct {
			Index int    `json:"index"`
			Team  string `json:"team"`
			File  string `json:"file"`
			Kind  string `json:"kind"`
		}{Index: i, Team: candTeamName, File: name, Kind: kinds[i]})
	}
	merged.Teams = mergedTeams
	if err := writeJSON("manifest.json", manifest); err != nil {
		fmt.Fprintf(stderr, "propose: %v\n", err)
		return 1
	}
	// Materialize one teams.yaml containing base teams + candidate teams.
	yamlData, err := yaml.Marshal(merged)
	if err != nil {
		fmt.Fprintf(stderr, "propose: marshal teams: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(*out, manifest.TeamsFile), yamlData, 0o644); err != nil {
		fmt.Fprintf(stderr, "propose: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "proposed %d candidates (%d general, %d domain) for team %q -> %s\n",
		len(candidates), len(general), len(domain), *team, *out)
	fmt.Fprintf(stdout, "base snapshot %s; serve %s and hot-reload before probing\n", base.ID[:12], manifest.TeamsFile)
	return 0
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// filterAdopted drops candidates whose every edit was already adopted
// according to the improvement harness (exact target+after match). It keeps
// kinds aligned with candidates and returns the drop count.
func filterAdopted(candidates []harness.EditSet, kinds []string, lessons []improvement.Lesson) ([]harness.EditSet, []string, int) {
	adopted := map[string]bool{}
	for _, l := range lessons {
		for _, e := range l.Edits {
			adopted[e.Target+"\x00"+e.After] = true
		}
	}
	var kept []harness.EditSet
	var keptKinds []string
	dropped := 0
	for i, es := range candidates {
		allAdopted := len(es.Edits) > 0
		for _, e := range es.Edits {
			if !adopted[e.Target+"\x00"+e.After] {
				allAdopted = false
				break
			}
		}
		if allAdopted {
			dropped++
			continue
		}
		kept = append(kept, es)
		keptKinds = append(keptKinds, kinds[i])
	}
	return kept, keptKinds, dropped
}
