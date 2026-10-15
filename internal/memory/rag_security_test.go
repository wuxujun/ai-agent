package memory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRAGSSERejectsUntrustedEndpoint(t *testing.T) {
	for _, endpoint := range []string{"https://attacker.test/messages", "http://169.254.169.254/metadata", "//example.test:444/messages", "http://example.test/messages", "https://user:password@example.test/messages", "/messages#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			posts := 0
			withRAGHTTPClient(t, func(r *http.Request) (int, string) {
				if r.Method == "GET" {
					return 200, "event: endpoint\ndata: " + endpoint + "\n\n"
				}
				posts++
				return 200, `{"jsonrpc":"2.0","result":{}}`
			})
			_, err := queryMCP(context.Background(), "https://example.test/sse", "test-token", "q")
			if err == nil || posts != 0 {
				t.Fatalf("endpoint executed: posts=%d err=%v", posts, err)
			}
		})
	}
}

func TestRAGRedirectDoesNotForwardCredentials(t *testing.T) {
	calls := 0
	original := ragHTTPClient
	ragHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://example.test:444/secret"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	defer func() { ragHTTPClient = original }()
	_, err := doRAGRequest(context.Background(), "https://example.test/search", "GET", "q", "test-token", false)
	if err == nil || calls != 1 {
		t.Fatalf("redirect followed: calls=%d err=%v", calls, err)
	}
}

func TestRAGCookiesStayWithinOneQuery(t *testing.T) {
	original := ragHTTPClient
	defer func() { ragHTTPClient = original }()
	gets, posts := 0, 0
	ragHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		body := `{"jsonrpc":"2.0","result":{}}`
		if r.Method == "GET" {
			gets++
			if r.Header.Get("Cookie") != "" {
				t.Error("cookie leaked from previous query")
			}
			h.Set("Set-Cookie", "session=one-query; Path=/")
			h.Set("Content-Type", "text/event-stream")
			body = "event: endpoint\ndata: /messages\n\n"
		} else {
			posts++
			if r.Header.Get("Cookie") != "session=one-query" {
				t.Error("handshake lost query-local cookie")
			}
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	for i := 0; i < 2; i++ {
		if _, err := queryMCP(context.Background(), "https://example.test/sse", "test-token", "q"); err != nil {
			t.Fatal(err)
		}
	}
	if gets != 2 || posts != 6 {
		t.Fatalf("gets=%d posts=%d", gets, posts)
	}
}

func TestRAGDefaultTransportRejectsPrivateAddress(t *testing.T) {
	// No test transport override: blocked before any connection is made.
	_, err := doRAGRequest(context.Background(), "http://127.0.0.1:1/search", "GET", "q", "", false)
	if err == nil || !strings.Contains(err.Error(), "private/restricted") {
		t.Fatalf("private endpoint not blocked: %v", err)
	}
}

func TestRAGLogOriginOmitsCredentialsAndQuery(t *testing.T) {
	if got := ragLogOrigin("https://user:password@example.test:444/path?token=secret#fragment"); got != "https://example.test:444" {
		t.Fatalf("unsafe origin: %q", got)
	}
}

func TestRAGTransportErrorOmitsQueryAndPreservesCancellation(t *testing.T) {
	original := ragHTTPClient
	defer func() { ragHTTPClient = original }()
	ragHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
	_, err := doRAGRequest(t.Context(), "https://example.test/search?token=sensitive-query", "GET", "q", "", false)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "sensitive-query") {
		t.Fatalf("unsafe error: %v", err)
	}
}
