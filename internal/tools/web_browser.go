package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type WebBrowserTool struct{}

func (t *WebBrowserTool) Name() string {
	return "web_browser"
}

func (t *WebBrowserTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *WebBrowserTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, Backoff: time.Second}
}

func (t *WebBrowserTool) Description() string {
	return "Load and render a webpage as readable text, stripping scripts, styles, and HTML tags."
}

func (t *WebBrowserTool) Parameters() map[string]any {
	return map[string]any{
		"url": map[string]any{"type": "string", "description": "Absolute URL of the webpage to load (must start with http:// or https://)"},
	}
}

func (t *WebBrowserTool) Validate(params map[string]any) error {
	raw, _ := params["url"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("web_browser requires non-empty url")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("web_browser url must start with http:// or https://")
	}
	return nil
}

func (t *WebBrowserTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	url, _ := params["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url parameter is required")
	}
	if err := policy.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("web_browser policy violation: %w", err)
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Limit reader to 1MB to prevent excessive memory usage or DoS
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}

	renderedText := renderHTML(string(body))

	return &ToolResult{
		Query:       url,
		Observation: fmt.Sprintf("Status %d:\n\n%s", resp.StatusCode, renderedText),
		Evidence: []types.Evidence{{
			Path:  url,
			Lines: []string{renderedText},
			Query: url,
		}},
	}, nil
}

func renderHTML(htmlContent string) string {
	// 1. Remove non-visible content tags (script, style, head, noscript, iframe)
	tagRegexes := []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script.*?>.*?</script>`),
		regexp.MustCompile(`(?is)<style.*?>.*?</style>`),
		regexp.MustCompile(`(?is)<head.*?>.*?</head>`),
		regexp.MustCompile(`(?is)<noscript.*?>.*?</noscript>`),
		regexp.MustCompile(`(?is)<iframe.*?>.*?</iframe>`),
	}
	for _, r := range tagRegexes {
		htmlContent = r.ReplaceAllString(htmlContent, "")
	}

	// 2. Format links: <a href="url">text</a> -> [text](url)
	linkRegex := regexp.MustCompile(`(?is)<a\s+[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	htmlContent = linkRegex.ReplaceAllString(htmlContent, "[$2]($1)")

	// 3. Remove all other HTML tags
	stripTagsRegex := regexp.MustCompile(`<[^>]*>`)
	text := stripTagsRegex.ReplaceAllString(htmlContent, "")

	// 4. Unescape HTML entities
	text = html.UnescapeString(text)

	// 5. Clean up whitespace and empty lines
	lines := strings.Split(text, "\n")
	var cleanedLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleanedLines = append(cleanedLines, line)
		}
	}

	return strings.Join(cleanedLines, "\n")
}

func init() {
	Register(&WebBrowserTool{})
}
