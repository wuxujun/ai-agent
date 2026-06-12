package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// toolMiddleware wraps a Tool to provide standard execution features:
// - Uniform timeout injection (from toolTimeout config)
// - Configurable retries on failure
// - Output truncation to avoid context window explosion
// - Evidence formatting standardization
type toolMiddleware struct {
	Tool
}

// Unwrap exposes the wrapped tool so callers (notably planner.ValidateDecision)
// can reach through to optional interfaces the underlying tool implements
// (e.g. planner.Validator) without making toolMiddleware aware of them.
func (m *toolMiddleware) Unwrap() Tool {
	return m.Tool
}

func (m *toolMiddleware) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	timeout := toolTimeout()
	retryPolicy := retryPolicyFor(m.Tool)

	var lastErr error
	var result *ToolResult

	// Fast fail for context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt <= retryPolicy.MaxRetries; attempt++ {
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		res, err := m.Tool.Execute(execCtx, workspace, params)
		cancel()

		if err == nil {
			result = res
			break
		}
		lastErr = err

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if attempt < retryPolicy.MaxRetries {
			log.Printf("[Tool %s] Execute failed (attempt %d): %v, retrying...", m.Name(), attempt+1, err)
			select {
			case <-time.After(retryPolicy.Backoff * time.Duration(attempt+1)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	if lastErr != nil && result == nil {
		return nil, fmt.Errorf("tool %s failed after %d attempts: %w", m.Name(), retryPolicy.MaxRetries+1, lastErr)
	}

	// 1. Truncate Observation
	if len(result.Observation) > 4000 {
		result.Observation = result.Observation[:4000] + "\n...[truncated by middleware]"
	}

	// 2. Standardize Evidence (if missing or poorly formatted)
	// We want to make sure multi-agent has uniform Evidence structure.
	if len(result.Evidence) == 0 {
		if m.Name() == "find_files" {
			// find.go doesn't produce evidence natively yet. But let's handle it here or modify find.go
			// Actually we will modify find.go to produce it properly.
		}
	}

	return result, nil
}

func retryPolicyFor(t Tool) RetryPolicy {
	if t.RiskLevel() == types.RiskLevelHigh {
		return RetryPolicy{}
	}

	provider, ok := t.(retryPolicyProvider)
	if !ok {
		return RetryPolicy{}
	}

	policy := provider.RetryPolicy()
	if policy.MaxRetries < 0 {
		policy.MaxRetries = 0
	}
	if policy.Backoff <= 0 {
		policy.Backoff = time.Second
	}
	return policy
}
