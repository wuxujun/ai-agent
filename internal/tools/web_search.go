package tools

import (
	"github.com/wuxujun/ai-agent/internal/types"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
	"net/url"
	"regexp"
	"strings"
)

type WebSearchTool struct{}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}


func (t *WebSearchTool) Description() string {
	return "Search the web using DuckDuckGo HTML"
}

func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"query": map[string]any{"type": "string", "description": "Search keywords"},
	}
}

func (t *WebSearchTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Ensure we do not load massive payloads, 512KB is enough for a DDG page
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}

	html := string(body)

	// Extract simple snippets (very rough HTML parsing)
	re := regexp.MustCompile(`(?i)<a class="result__snippet[^>]*>(.*?)</a>`)
	matches := re.FindAllStringSubmatch(html, 5)

	var results []string
	for _, match := range matches {
		if len(match) > 1 {
			// Strip HTML tags
			text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(match[1], "")
			results = append(results, "- "+strings.TrimSpace(text))
		}
	}

	observation := strings.Join(results, "\n")
	if observation == "" {
		observation = "no results found or parsing failed"
	}

	return &ToolResult{
		Query:       query,
		Observation: observation,
	}, nil
}

func init() {
	Register(&WebSearchTool{})
}
