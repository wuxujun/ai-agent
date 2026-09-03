# Brain Eval P0 Verification Record

UTC run time: `2026-09-03T01:56:45Z`

## Git Commit

- Evaluated commit: `ee7be3dc33d093700e780d6939e960cd6ab1e4f0`
- Branch: `feat/brain-eval-p0`

## Dataset

- Contract version: `3`
- Cases: `24`
- Isolated tenant/project scopes: `4`
- Dataset SHA-256: `d130aec473e6754a836b135e00f2fdfe53e148fa4eeff6eedd97b9f622872dc4`

## Verification Commands

| Command | Exit code |
|---|---:|
| `go test ./internal/braineval ./cmd/brain-eval -count=1` | 0 |
| `go test -race ./internal/braineval ./cmd/brain-eval -count=1` | 0 |
| `go test ./...` | 0 |
| `go vet ./...` | 0 |

The full repository test was rerun outside the restricted filesystem/network sandbox because local `httptest` listeners cannot bind inside that sandbox. The unrestricted rerun completed with exit code 0. `git diff --check` and the Brain Eval CLI build also completed with exit code 0.

## Offline Deterministic Evidence Metrics

Offline command: `brain-eval -input evals/brain/dataset.yaml -mode offline -format json` (exit code 0).

| Metric | Baseline | Brain | Delta/Result |
|---|---:|---:|---:|
| Comparable cases | 24/24 | 24/24 | complete |
| Errors | 0 | 0 | pass |
| Evidence recall | 0.100000 | 0.966667 | +0.866667 |
| Exact evidence URI recall | 0.000000 | 0.958333 | +0.958333 |
| Wiki citation coverage | 0.000000 | 1.000000 | +1.000000 |
| Fresh claim recall | 0.000000 | 1.000000 | +1.000000 |
| No-answer retrieval false-positive rate | 0.000000 | 0.000000 | pass |

Offline paired gate: `PASS`.

## Live LLM Answer Metrics

Provider scene configuration was frozen to Gemini `gemini-3.5-flash-lite`. The matched run used three repetitions for both arms and completed with 288 Writer/Judge calls. No answer, prompt, credential, private path, or raw provider response is retained here.

Sanitized Live command (exit code 0):

```text
brain-eval -input evals/brain/dataset.yaml -mode live -format json -repetitions 3 -max-total-tokens 60000 -max-total-cost-usd 1.803263
```

`-max-total-tokens 60000` was the user-approved conservative pre-call admission ceiling. The separate post-run P1 acceptance criterion remained actual usage `<= 50000` tokens.

| Metric | Baseline | Brain | Delta/Ratio |
|---|---:|---:|---:|
| Comparable cases | 24/24 | 24/24 | complete |
| Errors | 0 | 0 | pass |
| Judge failures | 0 | 0 | pass |
| Answer accuracy | 0.064516 | 0.290323 | +0.225806 |
| P95 latency | 1129 ms | 1213 ms | 1.074189x |
| Median-arm total tokens | 7614 | 8669 | 1.138561x |
| Median-arm estimated cost | $0.0076632 | $0.0083031 | +$0.0006399 |
| No-answer retrieval false-positive rate | 0.000000 | 0.000000 | pass |
| No-answer answer-hallucination rate | 0.000000 | 0.000000 | pass |
| Scope leaks | 0 | 0 | pass |
| Entity contaminations | 0 | 0 | pass |
| Retraction recurrences | 0 | 0 | pass |
| Prompt-injection recurrences | 0 | 0 | pass |

Live paired gate: `PASS` under the version 3 hard token ratio threshold of `1.20`. The non-blocking `1.10` optimization target was not met.

## Paired Regressions and Improvements

Improvements:

- Evidence recall, exact URI recall, citation coverage, Wiki citation coverage, fresh-claim recall, and answer accuracy improved.
- All nine critical supersession, isolation, retraction, and prompt-injection cases moved from Baseline critical failure to Brain pass.
- Stability improved for `decision_runtime_go` and `isolation_atlas_archive`.

Observed regressions:

- P95 latency ratio was `1.074189`; latency remains an observed, non-blocking P0 metric.
- Total token ratio was `1.138561`; it passed the `1.20` hard gate but missed the `1.10` optimization target.
- Estimated cost increased by `$0.0006399` across the median-arm summaries; cost remains within budget.
- Brain instability was observed for `preference_language_zh`, `preference_summary_concise`, `decision_store_sqlite`, `synthesis_release_brief`, `synthesis_risk_owner`, `synthesis_meeting_plan`, `synthesis_launch_dependencies`, `isolation_person_name`, `scope_cross_tenant`, `scope_cross_project`, and `retraction_vendor`. Instability is reported but is not a P0 hard gate.

## Budget and Stability

- Final three-repetition actual usage: `33,751` prompt + `15,246` completion = `48,997` total tokens.
- Final run estimated cost: `$0.0482403`.
- Final run actual acceptance ceiling: `50,000` tokens; result: pass.
- User-approved conservative admission ceiling: `60,000` tokens. This headroom allowed fail-closed pre-call reservation while preserving the separate 50,000-token actual acceptance condition.
- Cumulative authorized ceiling across diagnostics and final run: `300,000` tokens and `$2.00`.
- Cumulative actual usage: `277,085` tokens and `$0.2449773`.
- Live execution: 24/24 comparable cases, 0 infrastructure errors, 0 Judge failures, 288 calls.

## P1 Gate: PASS

Brain satisfies the version 3 P1 gate: deterministic evidence quality improved, Live answer accuracy improved by more than 0.10, total-token ratio remained at or below 1.20, actual final-run usage remained below 50,000 tokens, and every safety, isolation, retraction, no-answer, and Judge hard gate passed.
