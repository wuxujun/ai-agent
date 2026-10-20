package multiagent

import (
	"strings"
	"sync"
)

// teamOverlay is an in-memory, per-task refined team configuration. It lets
// the refinement canary serve an adopted improvement-harness lesson to a
// single task without rewriting teams.yaml or affecting concurrent tasks.
// Overlays are ephemeral: they live only for the duration of one
// Coordinator.Run call (registered before, cleared after) and are never
// persisted. Restarting the process drops them; lessons remain the durable
// record and are re-applied on the next run.
type teamOverlay struct {
	team string
	cfg  TeamConfig
}

var teamOverlays = struct {
	sync.RWMutex
	m map[string]teamOverlay // task ID -> overlay
}{m: map[string]teamOverlay{}}

// SetTeamOverlay registers a refined team config for one task ID.
func SetTeamOverlay(taskID, team string, cfg TeamConfig) {
	if strings.TrimSpace(taskID) == "" {
		return
	}
	teamOverlays.Lock()
	defer teamOverlays.Unlock()
	teamOverlays.m[strings.TrimSpace(taskID)] = teamOverlay{team: team, cfg: cfg}
}

// ClearTeamOverlay removes the overlay for a task ID. It is a no-op when
// none is registered.
func ClearTeamOverlay(taskID string) {
	teamOverlays.Lock()
	defer teamOverlays.Unlock()
	delete(teamOverlays.m, strings.TrimSpace(taskID))
}

func getTeamOverlay(taskID string) (teamOverlay, bool) {
	teamOverlays.RLock()
	defer teamOverlays.RUnlock()
	ov, ok := teamOverlays.m[strings.TrimSpace(taskID)]
	return ov, ok
}

// applyTeamOverlay swaps the active team's config for the overlaid one when
// the overlay's team matches. It is consulted by teamsConfigForTask so the
// refined harness flows through the normal config resolution path
// (snapshots, digests, lifecycle checks).
func applyTeamOverlay(taskID string, cfg *TeamsConfig) {
	if cfg == nil {
		return
	}
	ov, ok := getTeamOverlay(taskID)
	if !ok || ov.team != cfg.ActiveTeam {
		return
	}
	cfg.Teams[cfg.ActiveTeam] = ov.cfg
}
