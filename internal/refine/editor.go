package refine

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Editor proposes candidate harness edits for one refinement step.
// It returns up to k general edits and up to k domain-specific edits,
// mirroring Harness^2's parallel recursion (k general + k domain candidates),
// plus the token usage of the proposal call.
//
// lessons are prior adopted improvements for the team. Editors should treat
// them as experience: the LLM editor renders them as prompt context, while
// simpler editors (MockEditor) may ignore them.
type Editor interface {
	Propose(ctx context.Context, base harness.Snapshot, team string, k int, lessons []improvement.Lesson) (general, domain []harness.EditSet, usage types.TokenUsage, err error)
}

// MockEditor is a deterministic, heuristic editor for pipeline testing only.
// It is NOT a real improver: it applies fixed, documented heuristics so the
// propose -> probe -> compose loop can be exercised without model credentials.
type MockEditor struct{}

// mockHeuristic is one deterministic edit generator.
type mockHeuristic struct {
	name   string
	domain bool
	build  func(teamCfgTeam string, base harness.Snapshot, team string) harness.EditSet
}

func (MockEditor) Propose(_ context.Context, base harness.Snapshot, team string, k int, _ []improvement.Lesson) ([]harness.EditSet, []harness.EditSet, types.TokenUsage, error) {
	if _, ok := base.Teams.Teams[team]; !ok {
		return nil, nil, types.TokenUsage{}, fmt.Errorf("team %q not found in snapshot", team)
	}
	if k < 1 {
		return nil, nil, types.TokenUsage{}, fmt.Errorf("k must be >= 1")
	}
	general := []mockHeuristic{
		{name: "plan-rigor", build: planRigorHeuristic},
		{name: "researcher-tools", build: researcherToolsHeuristic},
		{name: "writer-verification", build: writerVerificationHeuristic},
	}
	domain := []mockHeuristic{
		{name: "executor-git", domain: true, build: executorGitHeuristic},
		{name: "critic-strictness", domain: true, build: criticStrictnessHeuristic},
	}
	mk := func(h mockHeuristic) harness.EditSet {
		return h.build(h.name, base, team)
	}
	var g, d []harness.EditSet
	for i := 0; i < k && i < len(general); i++ {
		g = append(g, mk(general[i]))
	}
	for i := 0; i < k && i < len(domain); i++ {
		d = append(d, mk(domain[i]))
	}
	return g, d, types.TokenUsage{}, nil
}

func editSet(base harness.Snapshot, name string, edits ...harness.Edit) harness.EditSet {
	return harness.EditSet{SnapshotID: base.ID, Author: "mock-editor:" + name, Edits: edits}
}

func planRigorHeuristic(name string, base harness.Snapshot, team string) harness.EditSet {
	cur := base.Teams.Teams[team].Routing.ReviewedMinPlanSteps
	return editSet(base, name, harness.Edit{
		Target: fmt.Sprintf("teams.%s.routing.reviewed_min_plan_steps", team),
		Op:     "set",
		Before: fmt.Sprint(cur),
		After:  fmt.Sprint(cur + 2),
		Reason: "mock: raise plan-review rigor by requiring longer plans",
	})
}

func researcherToolsHeuristic(name string, base harness.Snapshot, team string) harness.EditSet {
	return editSet(base, name, harness.Edit{
		Target: fmt.Sprintf("teams.%s.researcher.tools", team),
		Op:     "add-tools",
		After:  `["search_text"]`,
		Reason: "mock: give the researcher text search for deeper evidence gathering",
	})
}

func writerVerificationHeuristic(name string, base harness.Snapshot, team string) harness.EditSet {
	cur := base.Teams.Teams[team].Writer.SystemPrompt
	add := "\n\nVerify every claim against retrieved evidence before finalizing the answer."
	return editSet(base, name, harness.Edit{
		Target: fmt.Sprintf("teams.%s.writer.system_prompt", team),
		Op:     "set",
		Before: cur,
		After:  cur + add,
		Reason: "mock: remind the writer to verify claims against evidence",
	})
}

func executorGitHeuristic(name string, base harness.Snapshot, team string) harness.EditSet {
	return editSet(base, name, harness.Edit{
		Target: fmt.Sprintf("teams.%s.executor.tools", team),
		Op:     "add-tools",
		After:  `["git_diff"]`,
		Reason: "mock(domain): let the executor inspect code diffs",
	})
}

func criticStrictnessHeuristic(name string, base harness.Snapshot, team string) harness.EditSet {
	cur := base.Teams.Teams[team].Critic.SystemPrompt
	add := "\n\nBe strict about step ordering risks; reject plans with unsequenced side effects."
	return editSet(base, name, harness.Edit{
		Target: fmt.Sprintf("teams.%s.critic.system_prompt", team),
		Op:     "set",
		Before: cur,
		After:  cur + add,
		Reason: "mock(domain): tighten the critic on ordering risks",
	})
}
