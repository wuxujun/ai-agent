package orchestrator

import (
	"context"
	"iter"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/adk/model"
)

type budgetedADKModel struct {
	delegate model.LLM
}

func (m budgetedADKModel) Name() string { return m.delegate.Name() }

func (m budgetedADKModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		cfg := llmcore.ConfigForScene(config.LLMSceneADK)
		if err := llmcore.ReserveTaskLLMCallForConfig(ctx, cfg); err != nil {
			llmcore.ObserveReliabilityContext(ctx, llmcore.ReliabilityEvent{Kind: llmcore.ReliabilityTaskBudgetRejected, Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model})
			yield(nil, err)
			return
		}
		started := time.Now()
		var usage types.TokenUsage
		var callErr error
		defer func() {
			cost := llmcore.EstimateCostUSD(cfg, usage)
			llmcore.RecordTaskLLMCost(ctx, cost)
			llmcore.ObserveContext(ctx, llmcore.CallEvent{Scene: cfg.Scene, Provider: cfg.Provider, Model: cfg.Model, Usage: usage, Duration: time.Since(started), Err: callErr, Attempts: 1, EstimatedCostUSD: cost})
		}()
		for response, err := range m.delegate.GenerateContent(ctx, req, stream) {
			callErr = err
			if response != nil && response.UsageMetadata != nil {
				metadata := response.UsageMetadata
				usage.PromptTokens = max(usage.PromptTokens, int(metadata.PromptTokenCount))
				usage.CompletionTokens = max(usage.CompletionTokens, int(metadata.CandidatesTokenCount)+int(metadata.ThoughtsTokenCount))
				usage.TotalTokens = max(usage.TotalTokens, int(metadata.TotalTokenCount))
			}
			if !yield(response, err) {
				return
			}
		}
	}
}
