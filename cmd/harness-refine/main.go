// Command harness-refine implements the offline Phase 1 MVP of recursive
// harnessing: Propose–Probe–Compose over an ai-agent harness.
//
// Subcommands:
//   - propose  generate candidate harness edits (EditSets) from a base snapshot
//   - probe    run eval cases against a live service for base vs candidates
//   - compose  rank probe results without ground truth and emit the refined harness
//   - run      propose -> probe -> compose in one go
//
// Editors: propose --editor mock (deterministic heuristics, default) or
// --editor llm (model-proposed edits via the harness_editor LLM scene;
// configure llm.scenes.harness_editor or the provider default credentials).
//
// Exit codes: 0 success, 1 execution failure, 2 usage/configuration errors,
// 3 no candidate was adopted (compose only). The run command treats 3 as
// successful convergence.
package main

import (
	"fmt"
	"io"
	"os"
)

const exitNoAdoption = 3

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "propose":
		return runPropose(args[1:], stdout, stderr)
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "compose":
		return runCompose(args[1:], stdout, stderr)
	case "run":
		return runAll(args[1:], stdout, stderr)
	case "lessons":
		return runLessons(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "harness-refine: unknown subcommand %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `harness-refine: offline recursive harness refinement (Phase 1 MVP)

Usage:
  harness-refine propose --teams-yaml teams.yaml --team software --k 3 --out runs/hr/<ts>
  harness-refine probe   --service-url http://127.0.0.1:8088 --input evals/harness_regression.yaml \
                         --candidates runs/hr/<ts> --out runs/hr/<ts>
  harness-refine compose --probe-results runs/hr/<ts>/probe-results.json --out runs/hr/<ts>
  harness-refine lessons list --lessons-dir var/improvement
  harness-refine lessons assessments list --lessons-dir var/improvement
  harness-refine run     --teams-yaml teams.yaml --team software --k 3 \
                         --service-url http://127.0.0.1:8088 --input evals/harness_regression.yaml \
                         --out runs/hr/<ts> --lessons-dir var/improvement
  harness-refine run     --rounds 3 --service-teams-yaml /srv/ai-agent/teams.yaml \
                         --team software --k 2 --editor llm \
                         --service-url http://127.0.0.1:8088 --out runs/hr/<ts> \
                         --lessons-dir var/improvement
                         # multi-round: each round's winner becomes the next base

Probe notes: point the service at the materialized teams.candidates.yaml
(written by propose) and hot-reload it (POST /api/config/reload) before
probing, or start a second service with that file. Task creation uses the
"team" override, so one service can serve base and all candidates.
Use an admin API key or a tenant whose allowed_multiagent_teams includes
the candidate teams.

Exit codes: 0 success, 1 execution failure, 2 usage/configuration error,
3 no candidate adopted (compose only; run treats this as convergence).`)
}
