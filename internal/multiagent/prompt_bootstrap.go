package multiagent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"gopkg.in/yaml.v3"
)

type PromptBootstrapSummary struct {
	Existing int
	Created  int
}

// LoadTeamsConfigStrict loads teams.yaml for startup synchronization. Unlike
// GetTeamsConfig, it returns parse and file errors instead of silently falling
// back to defaults.
func LoadTeamsConfigStrict() (*TeamsConfig, error) {
	paths := []string{"teams.yaml", "../teams.yaml", "../../teams.yaml"}
	var lastErr error
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		var cfg TeamsConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if len(cfg.Teams) == 0 {
			return nil, fmt.Errorf("%s defines no teams", path)
		}
		return &cfg, nil
	}
	return nil, fmt.Errorf("load teams.yaml: %w", lastErr)
}

// TeamPromptSeeds returns every explicitly named team prompt in deterministic
// order. Inline-only prompts stay local until prompt_name is configured.
func TeamPromptSeeds(cfg *TeamsConfig) ([]promptmanager.Seed, error) {
	if cfg == nil {
		return nil, fmt.Errorf("teams config is nil")
	}
	teamNames := make([]string, 0, len(cfg.Teams))
	for name := range cfg.Teams {
		teamNames = append(teamNames, name)
	}
	sort.Strings(teamNames)

	byName := make(map[string]promptmanager.Seed)
	for _, teamName := range teamNames {
		team := cfg.Teams[teamName]
		roles := []struct {
			name   string
			config AgentConfig
		}{
			{name: "planner", config: team.Planner},
			{name: "critic", config: team.Critic},
			{name: "executor", config: team.Executor},
			{name: "verifier", config: team.Verifier},
			{name: "researcher", config: team.Researcher},
			{name: "writer", config: team.Writer},
		}
		for _, role := range roles {
			if err := addPromptSeed(byName, teamName+"/"+role.name, role.config); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(team.Verifier.DraftPromptName) != "" {
			if err := addPromptSeed(byName, teamName+"/verifier-draft", draftAgentConfig(team.Verifier)); err != nil {
				return nil, err
			}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	seeds := make([]promptmanager.Seed, 0, len(names))
	for _, name := range names {
		seeds = append(seeds, byName[name])
	}
	return seeds, nil
}

func addPromptSeed(byName map[string]promptmanager.Seed, location string, agent AgentConfig) error {
	name := strings.TrimSpace(agent.PromptName)
	if name == "" {
		name = strings.TrimSpace(agent.LangfusePrompt)
	}
	if name == "" {
		return nil
	}
	selector, err := agentPromptSelector(agent).Normalize()
	if err != nil {
		return fmt.Errorf("team prompt %s: %w", location, err)
	}
	candidate := promptmanager.Seed{
		Name: name, Content: strings.TrimSpace(agent.SystemPrompt), Selector: selector,
	}
	existing, ok := byName[name]
	if !ok {
		byName[name] = candidate
		return nil
	}
	if existing.Selector != candidate.Selector {
		return fmt.Errorf("Langfuse prompt %q uses conflicting selectors at %s", name, location)
	}
	switch {
	case existing.Content == "":
		existing.Content = candidate.Content
		byName[name] = existing
	case candidate.Content != "" && existing.Content != candidate.Content:
		return fmt.Errorf("Langfuse prompt %q uses conflicting local content at %s", name, location)
	}
	return nil
}

func BootstrapTeamPrompts(ctx context.Context, cfg *TeamsConfig) (PromptBootstrapSummary, error) {
	seeds, err := TeamPromptSeeds(cfg)
	if err != nil {
		return PromptBootstrapSummary{}, err
	}
	var summary PromptBootstrapSummary
	for _, seed := range seeds {
		resolved, created, err := promptmanager.GetManager().EnsureTextPrompt(ctx, seed)
		if err != nil {
			return summary, fmt.Errorf("bootstrap prompt %q: %w", seed.Name, err)
		}
		if resolved.Version <= 0 {
			return summary, fmt.Errorf("bootstrap prompt %q returned invalid version %d", seed.Name, resolved.Version)
		}
		if created {
			summary.Created++
		} else {
			summary.Existing++
		}
	}
	return summary, nil
}
