package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/types"
)

// mwLog is the structured logger for toolMiddleware. Using logger.Component
// ensures retry warnings are subject to the same log-level filtering and
// OTel correlation as every other package in the project.
var mwLog = logger.Component("tools.middleware")

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
			mwLog.Warn("tool execute failed, retrying",
				"tool", m.Name(),
				"attempt", attempt+1,
				"max_retries", retryPolicy.MaxRetries,
				"error", err,
			)
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

	// 1. Normalize and truncate Observation. Tool output can contain arbitrary
	// bytes, and slicing a UTF-8 sequence at byte 4000 used to leave an invalid
	// leading byte immediately before the truncation marker. PostgreSQL rejects
	// such trace text with SQLSTATE 22021.
	result.Observation = truncateToolObservation(result.Observation, 4000)

	// 2. Evidence standardization is handled per-tool in their Execute implementations.
	// No middleware-level override needed at this time.

	return result, nil
}

func truncateToolObservation(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + "\n...[truncated by middleware]"
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
