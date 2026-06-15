package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func (t *HttpFetchTool) Validate(params map[string]any) error {
	raw, _ := params["url"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("http_fetch requires non-empty url")
	}
	// Fail fast on obviously invalid schemes; full SSRF gating still happens at
	// Execute via policy.ValidateURL (which also resolves the host to check for
	// private/loopback addresses).
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("http_fetch url must start with http:// or https://")
	}
	return nil
}

func (t *HttpFetchTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	url, _ := params["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url parameter is required")
	}
	if err := policy.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("http_fetch policy violation: %w", err)
	}

	client := policy.SafeHTTPClient(15 * time.Second)

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
