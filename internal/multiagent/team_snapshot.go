package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

const teamConfigChangedReason = "team_config_changed"

type persistedTeamConfig struct {
	ActiveTeam string `json:"active_team"`
	Digest     string `json:"digest"`
}

type teamConfigChange struct {
	Previous persistedTeamConfig `json:"previous"`
	Current  persistedTeamConfig `json:"current"`
	Policy   ResumeConfigPolicy  `json:"policy"`
}

func persistedTeamConfigFromTask(task *types.Task) (persistedTeamConfig, bool) {
	if task == nil {
		return persistedTeamConfig{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		for _, evidence := range task.Trace[i].Evidence {
			if evidence.Path != "team_config" {
				continue
			}
			for _, line := range evidence.Lines {
				if digest := strings.TrimSpace(strings.TrimPrefix(line, "digest:")); digest != line && digest != "" {
					return persistedTeamConfig{ActiveTeam: evidence.Query, Digest: digest}, true
				}
			}
		}
	}
	return persistedTeamConfig{}, false
}

func enforceTeamConfigResumePolicy(task *types.Task, snapshot teamConfigSnapshot) error {
	previous, ok := persistedTeamConfigFromTask(task)
	if !ok || (previous.ActiveTeam == snapshot.ActiveTeam && previous.Digest == snapshot.Digest) {
		return nil
	}
	current := persistedTeamConfig{ActiveTeam: snapshot.ActiveTeam, Digest: snapshot.Digest}
	change := teamConfigChange{Previous: previous, Current: current, Policy: snapshot.ResumePolicy}
	observation, _ := json.Marshal(change)
	trace := types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      TeamConfigChangeTraceAction,
		Query:       string(snapshot.ResumePolicy),
		Observation: string(observation),
		AgentRole:   RolePlanner,
	}
	if snapshot.ResumePolicy == ResumeConfigUseLatest {
		trace.Evidence = []types.Evidence{{
			Path:  "team_config",
			Query: current.ActiveTeam,
			Lines: []string{"digest:" + current.Digest},
		}}
	}
	task.Trace = append(task.Trace, trace)
	task.StepCount++
	if snapshot.ResumePolicy == ResumeConfigRequireMatch {
		task.Status = types.StatusPartial
		appendUnresolvedReason(task, teamConfigChangedReason)
		return fmt.Errorf("multi-agent team configuration changed: previous team=%q digest=%s, current team=%q digest=%s", previous.ActiveTeam, previous.Digest, current.ActiveTeam, current.Digest)
	}
	removeUnresolvedReason(task, teamConfigChangedReason)
	return nil
}

// HasPendingTeamConfigChange reports whether a strict resume policy blocked a
// task until an operator explicitly selects use_latest.
func HasPendingTeamConfigChange(task *types.Task) bool {
	if task == nil {
		return false
	}
	for _, reason := range task.Unresolved {
		if reason == teamConfigChangedReason {
			_, ok := persistedTeamConfigFromTask(task)
			return ok
		}
	}
	return false
}
