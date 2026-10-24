package refine

import (
	"context"
	"fmt"
	"math"

	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

const budgetedEditorMaxOutputTokens = 4096

// This is a per-proposal reservation against configured token prices, not a
// shared account ledger or a guarantee about the provider's eventual invoice.
// It uses the same byte/schema/framing input bound as the Brain compiler.
func boundedEditorConfig(ctx context.Context, cfg llm.Config, system, user string, schema map[string]any, maxCost float64) (llm.Config, int, error) {
	for _, price := range []float64{cfg.InputCostPerMillionUSD, cfg.OutputCostPerMillionUSD} {
		// Config represents unspecified pricing as zero, so free and missing
		// prices cannot be distinguished here. Fail closed for budgeted calls.
		if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return llm.Config{}, 0, fmt.Errorf("llm editor cost ceiling requires positive finite input and output prices")
		}
	}
	cfg.MaxOutputTokens = minPositiveEditorLimit(cfg.MaxOutputTokens, budgetedEditorMaxOutputTokens)
	if limit := llm.MaxOutputTokensFromContext(ctx); limit > 0 {
		cfg.MaxOutputTokens = min(cfg.MaxOutputTokens, limit)
	}
	inputBound, err := llm.ConservativeInputTokenUpperBound(system, user, schema)
	if err != nil {
		return llm.Config{}, 0, fmt.Errorf("llm editor cost ceiling: cannot bound input")
	}
	reserved := llm.EstimateCostUSD(cfg, types.TokenUsage{PromptTokens: inputBound, CompletionTokens: cfg.MaxOutputTokens})
	if math.IsNaN(reserved) || math.IsInf(reserved, 0) || reserved > maxCost {
		return llm.Config{}, 0, fmt.Errorf("llm editor reserved cost $%.6f exceeds ceiling $%.6f", reserved, maxCost)
	}
	cfg.MaxRetries = 0
	cfg.FallbackScene = ""
	return cfg, inputBound, nil
}

func minPositiveEditorLimit(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	return min(limit, fallback)
}

func validEditorBudgetUsage(usage types.TokenUsage, inputBound, outputBound int) bool {
	if usage.PromptTokens <= 0 || usage.CompletionTokens <= 0 || usage.TotalTokens <= 0 || usage.PromptTokens > inputBound || usage.CompletionTokens > outputBound || usage.PromptTokens > math.MaxInt-usage.CompletionTokens {
		return false
	}
	return usage.TotalTokens == usage.PromptTokens+usage.CompletionTokens
}
