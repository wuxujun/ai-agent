// Package harness provides an explicit, versioned abstraction over the
// "harness": everything around the model that enables it to act as an agent
// (prompts, tool interfaces, routing, critic policy, skills).
//
// A harness is the system around a model that enables it to act as an agent;
// "harnessing" is the process of optimizing that system. This package is the
// Phase 0 foundation for recursive harnessing (see the design doc): it makes
// the harness a first-class, versioned value instead of scattered config.
//
// Scope (Phase 0):
//   - Capture: snapshot the current harness (teams.yaml + skills/) with a
//     content digest.
//   - Diff: compute the typed edits between two snapshots (for review and as
//     evidence).
//   - Apply: apply a validated EditSet to a snapshot to obtain a candidate
//     snapshot (pure function, no disk writes).
//
// It is purely additive: it wraps the existing multiagent.TeamsConfig and the
// skills registry without changing any runtime behavior.
//
// Edit target grammar (whitelisted, see validTargets):
//   - teams.<team>.<role>.<field>
//     role  ∈ {planner, critic, executor, verifier, researcher, writer}
//     field ∈ {system_prompt, prompt_name, langfuse_prompt, prompt_label,
//     prompt_version, provider, model, llm_scene, tools}
//   - teams.<team>.routing.<field>
//     field ∈ {reviewed_min_plan_steps, reviewed_min_remaining_llm_calls,
//     reviewed_min_remaining_tokens}
//
// Deliberately NOT editable here: security policy boundaries (workspace,
// URL allowlists), the allow_research_high_risk_tools routing flag, team
// lifecycle, and runtime selection. Those stay under human/operator control.
package harness
