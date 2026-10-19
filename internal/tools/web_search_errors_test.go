package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/logger"
)

type searchErrorTransport func(*http.Request) (*http.Response, error)

func (f searchErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebSearchErrorsExcludeUpstreamContent(t *testing.T) {
	const secret = "search-sensitive-sentinel"
	directory := t.TempDir()
	if err := logger.Configure(logger.Options{Level: "warn", FileEnabled: true, Directory: directory}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close(); logger.Reinit("info") })
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		transportErr error
	}{
		{name: "unauthorized", status: 401, body: secret},
		{name: "rate_limit", status: 429, body: secret},
		{name: "server_html", status: 500, body: "<html>" + secret + "</html>"},
		{name: "business_error", status: 200, body: `{"success":false,"error":"` + secret + `"}`},
		{name: "invalid_json", status: 200, body: secret},
		{name: "transport", transportErr: errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", "https://fixture.invalid/firecrawl?token="+secret, strings.NewReader(`{}`))
			closed := false
			client := &http.Client{Transport: searchErrorTransport(func(*http.Request) (*http.Response, error) {
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, Body: &searchCloseBody{Reader: strings.NewReader(tc.body), closed: &closed}, Header: make(http.Header)}, nil
			})}
			_, err := executeWebSearchRequest(client, req, true, "fixture")
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe error: %v", err)
			}
			if tc.transportErr == nil && !closed {
				t.Fatal("response not closed")
			}
			_, retryErr := (&toolMiddleware{Tool: &searchFailureTool{cause: err}}).Execute(t.Context(), "", nil)
			if retryErr == nil || strings.Contains(retryErr.Error(), secret) {
				t.Fatalf("unsafe retry error: %v", retryErr)
			}
		})
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatal("upstream content leaked into retry log")
		}
		seen = seen || strings.Contains(string(data), "tool execute failed, retrying")
	}
	if !seen {
		t.Fatal("retry log was not exercised")
	}
}

type searchFailureTool struct {
	WebSearchTool
	cause error
}

func (t searchFailureTool) Execute(context.Context, string, map[string]interface{}) (*ToolResult, error) {
	return nil, t.cause
}
func (t searchFailureTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 1, Backoff: time.Nanosecond}
}

type searchCloseBody struct {
	io.Reader
	closed *bool
}

func (b *searchCloseBody) Close() error { *b.closed = true; return nil }

func TestWebSearchCancellationPreservesIdentity(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://fixture.invalid/", nil)
		client := &http.Client{Transport: searchErrorTransport(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("sensitive: %w", cause) })}
		_, err := executeWebSearchRequest(client, req, false, "test")
		if err != cause {
			t.Fatalf("got %v want %v", err, cause)
		}
	}
}

// Uses the production HTTP response path against the saved local failure
// contract. URL policy remains tested separately and is never weakened.
func TestWebSearchErrorSimulation(t *testing.T) {
	base := os.Getenv("AI_AGENT_TEST_SEARCH_SIM_URL")
	if base == "" {
		t.Skip("local Postman simulation not requested")
	}
	for _, path := range []string{"401", "429", "500", "business"} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", base+"/firecrawl/"+path, strings.NewReader(`{"query":"fixture","limit":5}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		_, err = executeWebSearchRequest(&http.Client{}, req, true, "fixture")
		if err == nil || strings.Contains(err.Error(), "search-sensitive-sentinel") {
			t.Fatalf("%s: %v", path, err)
		}
	}
}
