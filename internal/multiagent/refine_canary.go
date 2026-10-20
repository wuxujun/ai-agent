package multiagent

import (
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// RefineSelectionTraceAction marks the task-trace entry recording the
// harness-refinement canary decision for a task.
const RefineSelectionTraceAction = "harness_refine_selection"

const refineSelectionVersion = 1

// RefineGate holds the resolved inputs for the harness-refinement canary
// decision. It mirrors the DAG canary's deterministic bucketing so operators
// get the same predictable rollout behavior.
type RefineGate struct {
	Enabled    bool
	Percent    int // 0-100, deterministic bucket by team+task ID
	DAGPercent int // DAG canary percent for the same team (never stacked with)
	Runtime    OrchestrationRuntime
	Team       string
	TaskID     string
}

// Bucketed reports whether the task runs with a refined harness.
// Refinements never stack with the DAG canary: a task bucketed for DAG,
// or a team configured for the DAG runtime, keeps the base harness so only
// one variable changes at a time.
func (g RefineGate) Bucketed() bool {
	if !g.Enabled || g.Percent <= 0 || g.Percent > 100 {
		return false
	}
	if strings.TrimSpace(g.TaskID) == "" {
		return false
	}
	if g.Runtime == RuntimeDAG {
		return false
	}
	if g.Runtime == RuntimeLegacy && g.DAGPercent > 0 && g.DAGPercent <= 100 &&
		stableRuntimeBucket(g.Team, g.TaskID) < g.DAGPercent {
		return false
	}
	return stableRuntimeBucket(g.Team, g.TaskID) < g.Percent
}

// RefineSelection is the recorded harness-refinement canary decision.
type RefineSelection struct {
	Version       int    `json:"version"`
	Team          string `json:"team"`
	LessonID      string `json:"lesson_id"`
	HarnessBefore string `json:"harness_before"`
	HarnessAfter  string `json:"harness_after"`
	Bucket        int    `json:"bucket"`
	Percent       int    `json:"percent"`
	Source        string `json:"source"` // "canary" | "persisted_canary"
}

// RefineBucket returns the deterministic canary bucket for a team+task ID,
// using the same scheme as the DAG canary.
func RefineBucket(team, taskID string) int {
	return stableRuntimeBucket(team, taskID)
}

// AppendRefineSelection records the canary decision in the task trace so it
// survives resume: a resumed task re-applies the same lesson instead of
// picking up a newer one, keeping the team-config digest stable.
func AppendRefineSelection(task *types.Task, team, lessonID, before, after string, bucket, percent int, persisted bool) {
	if task == nil {
		return
	}
	source := "canary"
	if persisted {
		source = "persisted_canary"
	}
	selection := RefineSelection{
		Version: refineSelectionVersion, Team: team,
		LessonID: lessonID, HarnessBefore: before, HarnessAfter: after,
		Bucket: bucket, Percent: percent, Source: source,
	}
	encoded, _ := json.Marshal(selection)
	task.Trace = append(task.Trace, types.StepTrace{
		Step: task.StepCount, Goal: task.Goal, Action: RefineSelectionTraceAction,
		Query: lessonID, Observation: string(encoded), AgentRole: RolePlanner,
	})
	task.StepCount++
}

// PersistedRefineSelection returns the canary decision recorded by an earlier
// run of the same task, if any.
func PersistedRefineSelection(task *types.Task, team string) (RefineSelection, bool) {
	if task == nil {
		return RefineSelection{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != RefineSelectionTraceAction {
			continue
		}
		var selection RefineSelection
		if json.Unmarshal([]byte(trace.Observation), &selection) != nil ||
			selection.Version != refineSelectionVersion || selection.Team != team {
			continue
		}
		return selection, true
	}
	return RefineSelection{}, false
}
