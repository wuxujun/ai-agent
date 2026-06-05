package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// toolMiddleware wraps a Tool to provide standard execution features:
// - Uniform timeout injection (from toolTimeout config)
// - Configurable retries on failure
// - Output truncation to avoid context window explosion
// - Evidence formatting standardization
type toolMiddleware struct {
	Tool
}

func (m *toolMiddleware) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	timeout := toolTimeout()

	var lastErr error
	var result *ToolResult

	maxRetries := 2

	// Fast fail for context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
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

		if attempt < maxRetries {
			log.Printf("[Tool %s] Execute failed (attempt %d): %v, retrying...", m.Name(), attempt+1, err)
			select {
			case <-time.After(time.Second * time.Duration(attempt+1)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	if lastErr != nil && result == nil {
		return nil, fmt.Errorf("tool %s failed after %d attempts: %w", m.Name(), maxRetries+1, lastErr)
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
