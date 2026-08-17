package multiagent_test

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
)

func TestCheckTeamRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configure   func(*config.Config)
		wantHealthy bool
		wantInvalid int
	}{
		{
			name: "valid active team and tenant allowlist",
			configure: func(cfg *config.Config) {
				cfg.MultiAgent.Team = "wiki"
				cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {AllowedMultiAgentTeams: []string{"wiki", "wiki_graph"}}}
			},
			wantHealthy: true,
		},
		{
			name: "missing active team",
			configure: func(cfg *config.Config) {
				cfg.MultiAgent.Team = "missing-active"
			},
			wantInvalid: 1,
		},
		{
			name: "missing tenant references",
			configure: func(cfg *config.Config) {
				cfg.MultiAgent.Team = "wiki"
				cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {AllowedMultiAgentTeams: []string{"missing-a", "missing-b"}}}
			},
			wantInvalid: 2,
		},
		{
			name: "missing tenant default",
			configure: func(cfg *config.Config) {
				cfg.MultiAgent.Team = "wiki"
				cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {DefaultMultiAgentTeam: "missing-default"}}
			},
			wantInvalid: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			tc.configure(cfg)
			health := multiagent.CheckTeamRouting(cfg)
			if health.Healthy != tc.wantHealthy || health.InvalidReferenceCount != tc.wantInvalid {
				t.Fatalf("health = %+v", health)
			}
			if health.TeamCount == 0 || !health.Configured {
				t.Fatalf("repository teams were not loaded: %+v", health)
			}
			if !tc.wantHealthy && !strings.Contains(health.Error, "unconfigured reference") {
				t.Fatalf("unexpected health error: %+v", health)
			}
		})
	}
}

func TestValidateTeamRoutingConfigReturnsActionableError(t *testing.T) {
	cfg := &config.Config{}
	cfg.MultiAgent.Team = "wiki"
	cfg.API.Tenants = map[string]config.APITenantConfig{
		"tenant-b": {AllowedMultiAgentTeams: []string{"wiki"}},
		"tenant-a": {AllowedMultiAgentTeams: []string{"missing-team"}},
	}
	err := multiagent.ValidateTeamRoutingConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), `api tenant "tenant-a" allows unconfigured Team "missing-team"`) {
		t.Fatalf("validation error = %v", err)
	}

	cfg.API.Tenants["tenant-a"] = config.APITenantConfig{AllowedMultiAgentTeams: []string{"wiki_graph"}}
	if err := multiagent.ValidateTeamRoutingConfig(cfg); err != nil {
		t.Fatalf("valid routing rejected: %v", err)
	}
}

func TestValidateTeamRoutingConfigRejectsTenantDefaultOutsideAllowlist(t *testing.T) {
	cfg := &config.Config{}
	cfg.MultiAgent.Team = "wiki"
	cfg.API.Tenants = map[string]config.APITenantConfig{
		"tenant-a": {DefaultMultiAgentTeam: "wiki_graph", AllowedMultiAgentTeams: []string{"wiki"}},
	}
	err := multiagent.ValidateTeamRoutingConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), `default Team "wiki_graph" is not included in its allowlist`) {
		t.Fatalf("validation error = %v", err)
	}
}
