package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// runAll chains propose -> probe -> compose, optionally for multiple rounds.
// Round 1 refines the given teams.yaml; each later round refines the previous
// round's winner (via its teams.refined.yaml). Rounds stop early when compose
// adopts nothing (converged).
//
// Multi-round probing needs the candidate teams on the service. With
// --service-teams-yaml, run deploys each round's teams.candidates.yaml to the
// service and hot-reloads it automatically, restoring the original file
// afterwards. Without it, the operator deploys candidates manually between
// phases (single-round usage).
func runAll(args []string, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	teamsYAML := fs.String("teams-yaml", "teams.yaml", "path to teams.yaml")
	skillsRoot := fs.String("skills-root", "skills", "skills directory")
	team := fs.String("team", "", "base team to refine (required)")
	k := fs.Int("k", 3, "candidates per scope")
	rounds := fs.Int("rounds", 1, "refinement rounds, 1-5 (each round's winner becomes the next base)")
	editorName := fs.String("editor", "mock", "editor: mock | llm")
	editorMaxCostUSD := fs.Float64("editor-max-cost-usd", 0, "ceiling for the llm editor call cost in USD (0 = no ceiling)")
	serviceURL := fs.String("service-url", "", "base URL of the ai-agent service (required)")
	serviceTeamsYAML := fs.String("service-teams-yaml", "", "service's teams.yaml path: auto-deploy candidates + hot-reload per round, restore afterwards")
	apiKey := fs.String("api-key", os.Getenv("AI_AGENT_API_KEY"), "X-API-Key (defaults to AI_AGENT_API_KEY)")
	input := fs.String("input", "evals/harness_regression.yaml", "YAML evaluation dataset")
	repetitions := fs.Int("repetitions", 1, "paired runs per case")
	reloadTimeout := fs.Duration("reload-timeout", 30*time.Second, "timeout for each configuration reload, including restoration")
	caseTimeout := fs.Duration("case-timeout", 3*time.Minute, "timeout per task")
	lessonsDir := fs.String("lessons-dir", "", "improvement harness directory (record lessons on adoption)")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *team == "" || *serviceURL == "" || *out == "" {
		fmt.Fprintln(stderr, "run: --team, --service-url and --out are required")
		return 2
	}
	if !validEditorCostCeiling(*editorMaxCostUSD) {
		fmt.Fprintln(stderr, "run: --editor-max-cost-usd must be finite and nonnegative")
		return 2
	}
	if *reloadTimeout <= 0 {
		fmt.Fprintln(stderr, "run: --reload-timeout must be positive")
		return 2
	}
	if *rounds < 1 || *rounds > 5 {
		fmt.Fprintln(stderr, "run: --rounds must be between 1 and 5")
		return 2
	}
	autoDeploy := *serviceTeamsYAML != ""
	if *rounds > 1 && !autoDeploy {
		fmt.Fprintln(stderr, "run: --rounds > 1 needs --service-teams-yaml for automatic candidate deployment + hot-reload")
		return 2
	}

	// Planned upper bound: each candidate gets its own paired base task.
	// Case count and actual proposal count are resolved by later phases.
	fmt.Fprintf(stdout, "run: up to %d round(s) x %d candidates x %d repetition(s) x 2 paired tasks per case (base + candidate)\n",
		*rounds, 2*(*k), *repetitions)

	var deployment *teamsDeployment
	if autoDeploy {
		var err error
		deployment, err = lockTeamsDeployment(*serviceTeamsYAML)
		if err != nil {
			fmt.Fprintf(stderr, "run: prepare service teams: %v\n", err)
			return 1
		}
		defer func() {
			if err := deployment.restore(*serviceURL, *apiKey, *reloadTimeout); err != nil {
				fmt.Fprintf(stderr, "run: restore service teams: %v\n", err)
				code = 1
			}
			if err := deployment.close(); err != nil {
				fmt.Fprintf(stderr, "run: release teams lock: %v\n", err)
				code = 1
			}
		}()
	}

	type iteration struct {
		Round      int    `json:"round"`
		Adopted    bool   `json:"adopted"`
		Winner     int    `json:"winner_index,omitempty"`
		WinnerTeam string `json:"winner_team,omitempty"`
		Reason     string `json:"reason"`
		RoundDir   string `json:"round_dir"`
	}
	var iterations []iteration
	baseYAML := *teamsYAML
	converged := false
	adoptedAny := false

	for r := 1; r <= *rounds; r++ {
		roundDir := filepath.Join(*out, fmt.Sprintf("round-%02d", r))
		fmt.Fprintf(stdout, "\n=== round %d/%d (base: %s) ===\n", r, *rounds, baseYAML)

		if code := runPropose([]string{
			"--teams-yaml", baseYAML, "--skills-root", *skillsRoot,
			"--team", *team, "--k", fmt.Sprint(*k),
			"--editor", *editorName, "--editor-max-cost-usd", fmt.Sprint(*editorMaxCostUSD),
			"--lessons-dir", *lessonsDir, "--out", roundDir,
		}, stdout, stderr); code != 0 {
			return code
		}
		manifestData, err := os.ReadFile(filepath.Join(roundDir, "manifest.json"))
		if err != nil {
			fmt.Fprintf(stderr, "run: read proposal manifest: %v\n", err)
			return 1
		}
		var manifest proposeManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			fmt.Fprintf(stderr, "run: parse proposal manifest: %v\n", err)
			return 1
		}
		if len(manifest.Candidates) == 0 {
			reason := "no new candidates to probe"
			iterations = append(iterations, iteration{Round: r, RoundDir: roundDir, Reason: reason})
			fmt.Fprintf(stdout, "round %d: %s, converged\n", r, reason)
			converged = true
			break
		}
		if autoDeploy {
			if err := deployment.deploy(filepath.Join(roundDir, "teams.candidates.yaml")); err != nil {
				fmt.Fprintf(stderr, "run: deploy candidates: %v\n", err)
				return 1
			}
			if err := reloadServiceTimeout(*serviceURL, *apiKey, *reloadTimeout); err != nil {
				fmt.Fprintf(stderr, "run: hot-reload service: %v\n", err)
				return 1
			}
		} else {
			fmt.Fprintf(stdout, "serve %s and hot-reload the service before probing\n",
				filepath.Join(roundDir, "teams.candidates.yaml"))
		}
		if code := runProbe([]string{
			"--service-url", *serviceURL, "--api-key", *apiKey,
			"--input", *input, "--candidates", roundDir,
			"--repetitions", fmt.Sprint(*repetitions),
			"--case-timeout", caseTimeout.String(),
			"--out", roundDir,
		}, stdout, stderr); code != 0 {
			return code
		}
		code := runCompose([]string{
			"--probe-results", filepath.Join(roundDir, "probe-results.json"),
			"--base-snapshot", filepath.Join(roundDir, "base-snapshot.json"),
			"--lessons-dir", *lessonsDir,
			"--out", roundDir,
		}, stdout, stderr)

		if code != 0 && code != exitNoAdoption {
			return code
		}
		result, err := readComposeOutput(filepath.Join(roundDir, "compose.json"))
		if err != nil {
			fmt.Fprintf(stderr, "run: read compose result: %v\n", err)
			return 1
		}
		it := iteration{Round: r, RoundDir: roundDir, Adopted: result.Adopted,
			Winner: result.WinnerIndex, WinnerTeam: result.WinnerTeam, Reason: result.Reason}
		iterations = append(iterations, it)

		if code == exitNoAdoption {
			// No candidate beat the base: converged.
			fmt.Fprintf(stdout, "round %d: no adoption, converged\n", r)
			converged = true
			break
		}
		adoptedAny = true
		baseYAML = filepath.Join(roundDir, "teams.refined.yaml")
	}

	summary := struct {
		Team          string      `json:"team"`
		RoundsPlanned int         `json:"rounds_planned"`
		Converged     bool        `json:"converged"`
		AdoptedAny    bool        `json:"adopted_any"`
		FinalTeams    string      `json:"final_teams_file,omitempty"`
		Iterations    []iteration `json:"iterations"`
	}{
		Team: *team, RoundsPlanned: *rounds,
		Converged: converged, AdoptedAny: adoptedAny, Iterations: iterations,
	}
	if adoptedAny {
		final := filepath.Join(*out, "teams.final.yaml")
		data, err := os.ReadFile(baseYAML)
		if err != nil {
			fmt.Fprintf(stderr, "run: read final teams: %v\n", err)
			return 1
		}
		if err := os.WriteFile(final, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "run: write final teams: %v\n", err)
			return 1
		}
		summary.FinalTeams = "teams.final.yaml"
		fmt.Fprintf(stdout, "\nrefined harness: %s (review before publishing)\n", final)
	}
	if err := writeJSONFile(filepath.Join(*out, "iterations.json"), summary); err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	if converged {
		fmt.Fprintf(stdout, "converged after %d round(s)\n", len(iterations))
	}
	return 0
}

// reloadService uses a bounded request even during restoration.
func reloadService(serviceURL, apiKey string) error {
	return reloadServiceTimeout(serviceURL, apiKey, 30*time.Second)
}

func reloadServiceTimeout(serviceURL, apiKey string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL+"/api/config/reload", nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func readComposeOutput(path string) (composeOutput, error) {
	var out composeOutput
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	return out, nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
