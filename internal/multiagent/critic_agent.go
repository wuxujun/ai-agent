package multiagent

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/types"
)

// CriticAgent is team-aware and delegates the actual structured review to the
// shared plan critic implementation.
type CriticAgent struct{}

func (c *CriticAgent) Critique(ctx context.Context, task *types.Task, plan plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	agentCfg := GetTeamsConfig().GetActiveTeam().Critic
	cfg := coreLLMConfig(agentCfg, config.LLMScenePlanCritic)
	systemPrompt := resolveAgentPrompt(ctx, agentCfg, "multiagent_critic_prompt", plancritic.DefaultSystemPrompt)
	return plancritic.NewLLMCriticWithConfig(cfg, systemPrompt).Critique(ctx, task, plan)
}

func coreLLMConfig(agentCfg AgentConfig, defaultScene string) llmcore.Config {
	roleCfg := GetLLMConfig(agentCfg, defaultScene)
	cfg := llmcore.ConfigForScene(roleCfg.Scene)
	cfg.Provider = string(roleCfg.Provider)
	cfg.APIKey = roleCfg.APIKey
	cfg.Model = roleCfg.Model
	cfg.BaseURL = roleCfg.BaseURL
	cfg.Timeout = roleCfg.Timeout
	cfg.FallbackScene = roleCfg.FallbackScene
	cfg.MaxRetries = roleCfg.MaxRetries
	return cfg
}
