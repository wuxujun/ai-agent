---
name: code-review
description: Review Go code in the workspace for bugs, missing error handling, and registry/schema sync issues before sign-off.
allowed-tools: search_text, read_file, git_diff
---

# Code Review

Use this skill when the goal asks you to review, audit, or sanity-check code in
the workspace.

## Steps

1. Run `git_diff` to see what changed. If there is no diff, fall back to
   `find_files` + `read_file` on the files named in the goal.
2. For each changed Go file, read it with `read_file` and check:
   - every returned `error` is handled (no silent `_ =` on a fallible call that
     matters),
   - exported functions have the behavior their doc comment claims,
   - no obvious nil-deref or unchecked type assertion (`x.(T)` without `, ok`).
3. If the change touches the tool registry or planner output shape, verify the
   three-way invariant: `tools.DefaultRegistry`, `PlannerDecisionSchema`,
   `PlannerDecisionGenAISchema`, and `ValidateDecision` must stay in lock-step.
   Use `search_text` for `DefaultRegistry`, `PlannerDecisionSchema`, and
   `ValidateDecision` to confirm all references agree.
4. Summarize findings as: blocking issues first, then nits. For each issue give
   file:line and a one-line fix suggestion.

## Stop condition

Stop and give the final answer once every changed file has been read and
checked. Do not modify files — this is a review-only skill.
