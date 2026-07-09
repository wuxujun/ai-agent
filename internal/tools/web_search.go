package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type WebSearchTool struct{}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *WebSearchTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, Backoff: time.Second}
}

func (t *WebSearchTool) Description() string {
	return "Search the web using the configured search provider (e.g. Firecrawl)"
}

func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"query": map[string]any{"type": "string", "description": "Search keywords"},
	}
}

func (t *WebSearchTool) Validate(params map[string]any) error {
	query, _ := params["query"].(string)
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("web_search requires non-empty query")
	}
	return nil
}

type firecrawlItem struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Snippet     string `json:"snippet"`
}

type firecrawlResponse struct {
	Success bool            `json:"success"`
	Data    []firecrawlItem `json:"data"`
	Error   string          `json:"error"`
}

func (t *WebSearchTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	cfg := config.Get()
	searchURL := cfg.Search.URL
	if searchURL == "" {
		searchURL = "https://api.firecrawl.dev/v1/search"
	}

	apiKey := cfg.Search.APIKey

	isFirecrawl := strings.Contains(strings.ToLower(searchURL), "firecrawl")

	var req *http.Request
	var err error

	if isFirecrawl {
		// Firecrawl uses POST request with JSON body
		if err := policy.ValidateURL(searchURL); err != nil {
			return nil, fmt.Errorf("web_search policy violation: %w", err)
		}

		payload := map[string]any{
			"query": query,
			"limit": 5,
		}
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal firecrawl payload: %w", err)
		}

		req, err = http.NewRequestWithContext(ctx, "POST", searchURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		// DuckDuckGo or other GET-based template search URL
		var targetURL string
		if strings.Contains(searchURL, "%s") {
			targetURL = fmt.Sprintf(searchURL, url.QueryEscape(query))
		} else if strings.Contains(searchURL, "?") {
			if strings.HasSuffix(searchURL, "?") || strings.HasSuffix(searchURL, "&") {
				targetURL = searchURL + "q=" + url.QueryEscape(query)
			} else {
				targetURL = searchURL + "&q=" + url.QueryEscape(query)
			}
		} else {
			targetURL = searchURL + "?q=" + url.QueryEscape(query)
		}

		if err := policy.ValidateURL(targetURL); err != nil {
			return nil, fmt.Errorf("web_search policy violation: %w", err)
		}

		req, err = http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	}

	client := policy.SafeHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("search request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// 1MB limit is plenty for structured JSON or HTML snippets
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}

	var observation string
	if isFirecrawl {
		var fcResp firecrawlResponse
		if err := json.Unmarshal(body, &fcResp); err != nil {
			return nil, fmt.Errorf("failed to decode firecrawl response: %w", err)
		}

		if fcResp.Error != "" {
			return nil, fmt.Errorf("firecrawl API error: %s", fcResp.Error)
		}

		var results []string
		for _, item := range fcResp.Data {
			text := item.Description
			if text == "" {
				text = item.Snippet
			}
			if text == "" {
				continue
			}
			results = append(results, fmt.Sprintf("- [%s](%s): %s", item.Title, item.URL, text))
		}
		observation = strings.Join(results, "\n")
	} else {
		// Parse DuckDuckGo HTML snippets (legacy behavior)
		html := string(body)
		re := regexp.MustCompile(`(?i)<a class="result__snippet[^>]*>(.*?)</a>`)
		matches := re.FindAllStringSubmatch(html, 5)

		var results []string
		for _, match := range matches {
			if len(match) > 1 {
				text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(match[1], "")
				results = append(results, "- "+strings.TrimSpace(text))
			}
		}
		observation = strings.Join(results, "\n")
	}

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
