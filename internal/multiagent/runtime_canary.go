package multiagent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

const runtimeSelectionVersion = 1

type runtimeSelection struct {
	Version    int                  `json:"version"`
	ActiveTeam string               `json:"active_team"`
	Runtime    OrchestrationRuntime `json:"runtime"`
	Source     string               `json:"source"`
	Bucket     int                  `json:"bucket"`
	Percent    int                  `json:"percent"`
}

func resolveTaskRuntime(task *types.Task, snapshot teamConfigSnapshot, configured OrchestrationRuntime) runtimeSelection {
	if persisted, ok := persistedRuntimeSelection(task, snapshot.ActiveTeam); ok {
		persisted.Source = "persisted_" + persisted.Source
		return persisted
	}
	selection := runtimeSelection{
		Version: runtimeSelectionVersion, ActiveTeam: snapshot.ActiveTeam,
		Runtime: parseOrchestrationRuntime(string(configured)), Source: "configured",
		Bucket: -1, Percent: snapshot.Team.DAGCanaryPercent,
	}
	if selection.Runtime == RuntimeLegacy && selection.Percent > 0 && selection.Percent <= 100 && task != nil && strings.TrimSpace(task.ID) != "" {
		selection.Bucket = stableRuntimeBucket(snapshot.ActiveTeam, task.ID)
		selection.Source = "canary"
		if selection.Bucket < selection.Percent {
			selection.Runtime = RuntimeDAG
		}
		appendRuntimeSelection(task, selection)
	}
	return selection
}

func stableRuntimeBucket(activeTeam, taskID string) int {
	digest := sha256.Sum256([]byte(strings.TrimSpace(activeTeam) + "\x00" + strings.TrimSpace(taskID)))
	return int(binary.BigEndian.Uint64(digest[:8]) % 100)
}

func appendRuntimeSelection(task *types.Task, selection runtimeSelection) {
	if task == nil {
		return
	}
	encoded, _ := json.Marshal(selection)
	task.Trace = append(task.Trace, types.StepTrace{
		Step: task.StepCount, Goal: task.Goal, Action: RuntimeSelectionTraceAction,
		Query: string(selection.Runtime), Observation: string(encoded), AgentRole: RolePlanner,
	})
	task.StepCount++
}

func persistedRuntimeSelection(task *types.Task, activeTeam string) (runtimeSelection, bool) {
	if task == nil {
		return runtimeSelection{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != RuntimeSelectionTraceAction {
			continue
		}
		var selection runtimeSelection
		if json.Unmarshal([]byte(trace.Observation), &selection) != nil || selection.Version != runtimeSelectionVersion || selection.ActiveTeam != activeTeam {
			continue
		}
		if selection.Runtime != RuntimeLegacy && selection.Runtime != RuntimeDAG {
			continue
		}
		return selection, true
	}
	return runtimeSelection{}, false
}
