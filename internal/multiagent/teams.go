package multiagent

import (
	"os"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/planner"
	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Name         string `yaml:"name" json:"name"`
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt"`
	Provider     string `yaml:"provider" json:"provider"`
	Model        string `yaml:"model" json:"model"`
	LLMScene     string `yaml:"llm_scene" json:"llm_scene"`
}

type TeamConfig struct {
	Planner    AgentConfig `yaml:"planner" json:"planner"`
	Researcher AgentConfig `yaml:"researcher" json:"researcher"`
	Writer     AgentConfig `yaml:"writer" json:"writer"`
}

type TeamsConfig struct {
	ActiveTeam string                `yaml:"active_team" json:"active_team"`
	Teams      map[string]TeamConfig `yaml:"teams" json:"teams"`
}

// GetTeamsConfig loads and parses teams.yaml if it exists.
// Hot-reloads on demand by not caching the result forever.
func GetTeamsConfig() *TeamsConfig {
	cfg := &TeamsConfig{
		ActiveTeam: "default",
		Teams:      make(map[string]TeamConfig),
	}

	// Try loading from teams.yaml in the current directory and parent directories
	paths := []string{"teams.yaml", "../teams.yaml", "../../teams.yaml"}
	var data []byte
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}

	if err == nil {
		if parseErr := yaml.Unmarshal(data, cfg); parseErr != nil {
			log.Error("Failed to parse teams.yaml", "error", parseErr)
		}
	}

	// If AI_AGENT_MULTIAGENT_TEAM environment variable is set, override the active team
	if envTeam := os.Getenv("AI_AGENT_MULTIAGENT_TEAM"); envTeam != "" {
		cfg.ActiveTeam = envTeam
	}

	return cfg
}

// GetActiveTeam returns the active TeamConfig. If the active team is not found,
// it returns an empty TeamConfig (which falls back to original hardcoded values).
func (c *TeamsConfig) GetActiveTeam() TeamConfig {
	if c.Teams == nil {
		return TeamConfig{}
	}
	if team, ok := c.Teams[c.ActiveTeam]; ok {
		return team
	}
	return TeamConfig{}
}

// GetLLMConfig returns an LLMConfig derived from AgentConfig, falling back
// to the default LLMConfig if fields are omitted.
func GetLLMConfig(agentCfg AgentConfig, defaultScene ...string) LLMConfig {
	scene := ""
	if len(defaultScene) > 0 {
		scene = defaultScene[0]
	}
	if agentCfg.LLMScene != "" {
		scene = agentCfg.LLMScene
	}
	cfg := LLMConfigForScene(scene)
	if agentCfg.Provider != "" {
		globalCfg := config.Get()
		resolved := globalCfg.ResolveLLMProviderConfig(agentCfg.Provider)
		cfg.Provider = planner.ProviderType(agentCfg.Provider)
		cfg.APIKey = resolved.APIKey
		cfg.BaseURL = resolved.BaseURL
		if agentCfg.Model == "" {
			cfg.Model = resolved.Model
		}
	}
	if agentCfg.Model != "" {
		cfg.Model = agentCfg.Model
	}
	return cfg
}
