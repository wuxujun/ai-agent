package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type HttpFetchTool struct{}

func (t *HttpFetchTool) Name() string {
	return "http_fetch"
}

func (t *HttpFetchTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *HttpFetchTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, Backoff: time.Second}
}

func (t *HttpFetchTool) Description() string {
	return "Fetch content from an HTTP/HTTPS URL"
}

func (t *HttpFetchTool) Parameters() map[string]any {
	return map[string]any{
		"url": map[string]any{"type": "string", "description": "Absolute http/https URL to fetch"},
	}
}

func (t *HttpFetchTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	url, _ := params["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url parameter is required")
	}
	if err := policy.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("http_fetch policy violation: %w", err)
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Use LimitReader to prevent DoS/OOM from extremely large files (e.g. infinite streams)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4000))
	if err != nil {
		return nil, err
	}

	content := string(body)

	return &ToolResult{
		Query:       url,
		Observation: fmt.Sprintf("Status %d: \n%s", resp.StatusCode, content),
	}, nil
}

func init() {
	Register(&HttpFetchTool{})
}
