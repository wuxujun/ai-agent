package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

// CriticAgent is team-aware and delegates the actual structured review to the
// shared plan critic implementation.
type CriticAgent struct{}

type criticPromptSelectorContextKey struct{}

type criticPromptSelectorOverride struct {
	Selector promptmanager.Selector
}

// WithCriticPromptSelector overrides only the Langfuse selector for a Critic
// call. The prompt name, model, provider, and fallback still come from the
// active team. It is primarily used for controlled prompt evaluations.
func WithCriticPromptSelector(ctx context.Context, selector promptmanager.Selector) (context.Context, error) {
	normalized, err := selector.Normalize()
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, criticPromptSelectorContextKey{}, criticPromptSelectorOverride{Selector: normalized}), nil
}

func (c *CriticAgent) Critique(ctx context.Context, task *types.Task, plan plancritic.Plan) (*plancritic.Result, types.TokenUsage, error) {
	agentCfg := teamConfigFromContext(ctx).Team.Critic
	var selectorOverride *promptmanager.Selector
	if ctx != nil {
		if override, ok := ctx.Value(criticPromptSelectorContextKey{}).(criticPromptSelectorOverride); ok {
			selectorOverride = &override.Selector
			agentCfg.PromptLabel = override.Selector.Label
			agentCfg.PromptVersion = override.Selector.Version
		}
	}
	cfg := coreLLMConfig(agentCfg, config.LLMScenePlanCritic)
	var resolvedPrompt resolvedAgentPrompt
	var err error
	if selectorOverride != nil {
		resolvedPrompt, err = resolveAgentPromptStrict(ctx, agentCfg, "multiagent_critic_prompt")
		if err != nil {
			return nil, types.TokenUsage{}, fmt.Errorf("resolve critic evaluation prompt: %w", err)
		}
	} else {
		resolvedPrompt, err = resolveAgentPromptDetailsForTask(ctx, agentCfg, "multiagent_critic_prompt", plancritic.DefaultSystemPrompt)
		if err != nil {
			return nil, types.TokenUsage{}, fmt.Errorf("resolve CriticAgent prompt: %w", err)
		}
	}
	callCtx := llmcore.WithPromptBinding(ctx, resolvedPrompt.Binding)
	return plancritic.NewLLMCriticWithConfig(cfg, resolvedPrompt.Content).Critique(callCtx, task, plan)
}

func resolveAgentPromptStrict(ctx context.Context, agentCfg AgentConfig, defaultName string) (resolvedAgentPrompt, error) {
	promptName := strings.TrimSpace(agentCfg.PromptName)
	if promptName == "" {
		promptName = strings.TrimSpace(agentCfg.LangfusePrompt)
	}
	if promptName == "" {
		if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
			return resolvedAgentPrompt{}, fmt.Errorf("prompt selector override requires prompt_name when system_prompt is configured")
		}
		promptName = defaultName
	}
	resolved, err := promptmanager.GetManager().ResolveStrict(ctx, promptName, agentPromptSelector(agentCfg))
	return resolvedAgentPromptFromResolution(resolved), err
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
