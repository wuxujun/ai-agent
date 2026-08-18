package multiagent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
)

func init() {
	config.RegisterReloadValidator("multi-agent Team routing", ValidateTeamRoutingConfig)
}

// TeamRoutingHealth is the public, non-sensitive readiness view for dynamic
// Team routing. It intentionally reports counts instead of tenant identifiers.
type TeamRoutingHealth struct {
	Configured            bool   `json:"configured"`
	Healthy               bool   `json:"healthy"`
	ActiveTeam            string `json:"active_team"`
	TeamCount             int    `json:"team_count"`
	InvalidReferenceCount int    `json:"invalid_reference_count"`
	Error                 string `json:"error"`
}

// CheckTeamRouting validates cross-file references that config.Validate cannot
// check without creating a config <-> multiagent import cycle.
func CheckTeamRouting(runtime *config.Config) TeamRoutingHealth {
	health := TeamRoutingHealth{}
	teams, err := LoadTeamsConfigStrict()
	if err != nil {
		health.Error = "Team configuration could not be loaded"
		return health
	}
	health.Configured = true
	health.TeamCount = len(teams.Teams)
	for _, team := range teams.Teams {
		if !validTeamLifecycle(team.Lifecycle) {
			health.InvalidReferenceCount++
		}
	}
	activeTeam := strings.TrimSpace(teams.ActiveTeam)
	if runtime != nil {
		if configured := strings.TrimSpace(runtime.MultiAgent.Team); configured != "" {
			activeTeam = configured
		}
	}
	health.ActiveTeam = activeTeam
	activeConfig, ok := teams.Teams[activeTeam]
	if activeTeam == "" || !ok || normalizeTeamLifecycle(activeConfig.Lifecycle) != TeamLifecycleActive {
		health.InvalidReferenceCount++
	}
	if runtime != nil {
		for _, tenant := range runtime.API.Tenants {
			if tenantDefault := strings.TrimSpace(tenant.DefaultMultiAgentTeam); tenantDefault != "" {
				team, ok := teams.Teams[tenantDefault]
				if !ok || normalizeTeamLifecycle(team.Lifecycle) != TeamLifecycleActive {
					health.InvalidReferenceCount++
				}
			}
			for _, team := range tenant.AllowedMultiAgentTeams {
				if _, ok := teams.Teams[team]; !ok {
					health.InvalidReferenceCount++
				}
			}
		}
	}
	if health.InvalidReferenceCount > 0 {
		health.Error = fmt.Sprintf("Team routing contains %d unconfigured reference(s)", health.InvalidReferenceCount)
		return health
	}
	health.Healthy = true
	return health
}

// ValidateTeamRoutingConfig returns an actionable operator error for config
// reload while CheckTeamRouting keeps the public readiness response redacted.
func ValidateTeamRoutingConfig(runtime *config.Config) error {
	teams, err := LoadTeamsConfigStrict()
	if err != nil {
		return err
	}
	teamNames := make([]string, 0, len(teams.Teams))
	for teamName := range teams.Teams {
		teamNames = append(teamNames, teamName)
	}
	sort.Strings(teamNames)
	for _, teamName := range teamNames {
		if !validTeamLifecycle(teams.Teams[teamName].Lifecycle) {
			return fmt.Errorf("Team %q lifecycle must be active, draining, or retired", teamName)
		}
	}
	activeTeam := strings.TrimSpace(teams.ActiveTeam)
	if runtime != nil {
		if configured := strings.TrimSpace(runtime.MultiAgent.Team); configured != "" {
			activeTeam = configured
		}
	}
	activeConfig, ok := teams.Teams[activeTeam]
	if activeTeam == "" || !ok {
		return fmt.Errorf("default Team %q is not configured", activeTeam)
	}
	if normalizeTeamLifecycle(activeConfig.Lifecycle) != TeamLifecycleActive {
		return fmt.Errorf("default Team %q is %s", activeTeam, normalizeTeamLifecycle(activeConfig.Lifecycle))
	}
	if runtime == nil {
		return nil
	}
	tenantIDs := make([]string, 0, len(runtime.API.Tenants))
	for tenantID := range runtime.API.Tenants {
		tenantIDs = append(tenantIDs, tenantID)
	}
	sort.Strings(tenantIDs)
	for _, tenantID := range tenantIDs {
		tenant := runtime.API.Tenants[tenantID]
		if tenantDefault := strings.TrimSpace(tenant.DefaultMultiAgentTeam); tenantDefault != "" {
			teamConfig, ok := teams.Teams[tenantDefault]
			if !ok {
				return fmt.Errorf("api tenant %q uses unconfigured default Team %q", tenantID, tenantDefault)
			}
			if normalizeTeamLifecycle(teamConfig.Lifecycle) != TeamLifecycleActive {
				return fmt.Errorf("api tenant %q uses %s default Team %q", tenantID, normalizeTeamLifecycle(teamConfig.Lifecycle), tenantDefault)
			}
			if len(tenant.AllowedMultiAgentTeams) > 0 && !containsExact(tenant.AllowedMultiAgentTeams, tenantDefault) {
				return fmt.Errorf("api tenant %q default Team %q is not included in its allowlist", tenantID, tenantDefault)
			}
		}
		for _, team := range tenant.AllowedMultiAgentTeams {
			if _, ok := teams.Teams[team]; !ok {
				return fmt.Errorf("api tenant %q allows unconfigured Team %q", tenantID, team)
			}
		}
	}
	return nil
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validTeamLifecycle(value TeamLifecycle) bool {
	normalized := strings.ToLower(strings.TrimSpace(string(value)))
	return normalized == "" || normalized == string(TeamLifecycleActive) || normalized == string(TeamLifecycleDraining) || normalized == string(TeamLifecycleRetired)
}
