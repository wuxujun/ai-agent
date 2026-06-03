package tools

import (
	"github.com/wuxujun/ai-agent/internal/types"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/wuxujun/ai-agent/internal/policy"
)

type HttpFetchTool struct{}

func (t *HttpFetchTool) Name() string {
	return "http_fetch"
}

func (t *HttpFetchTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
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

	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	content := string(body)
	if len(content) > 4000 {
		content = content[:4000] // truncate to fit in observation
	}

	return &ToolResult{
		Query:       url,
		Observation: fmt.Sprintf("Status %d: \n%s", resp.StatusCode, content),
	}, nil
}

func init() {
	Register(&HttpFetchTool{})
}
