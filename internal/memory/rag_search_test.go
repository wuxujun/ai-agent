package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withRAGHTTPClient(t *testing.T, fn func(*http.Request) (int, string)) {
	t.Helper()
	original := ragHTTPClient
	ragHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			status, body := fn(req)
			header := make(http.Header)
			if strings.Contains(body, "event:") || strings.Contains(body, "data:") {
				header.Set("Content-Type", "text/event-stream")
			} else {
				header.Set("Content-Type", "application/json")
			}
			return &http.Response{
				StatusCode: status,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
	t.Cleanup(func() { ragHTTPClient = original })
}

func TestSearchThirdPartyRAG_GET(t *testing.T) {
	ctx := context.Background()

	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		if r.Method != "GET" {
			t.Errorf("expected GET request, got %s", r.Method)
		}
		qVal := r.URL.Query().Get("q")
		if qVal != "test-query" {
			t.Errorf("expected query parameter q='test-query', got %q", qVal)
		}
		accept := r.Header.Get("Accept")
		if accept != "application/json, text/event-stream" {
			t.Errorf("expected Accept header 'application/json, text/event-stream', got %q", accept)
		}

		response := []map[string]any{
			{
				"id":           "mem-ext-99",
				"goal":         "find database configuration",
				"key_findings": "found main config in config/db.yaml",
				"final_answer": "config/db.yaml is the main database config",
			},
		}

		b, _ := json.Marshal(response)
		return http.StatusOK, string(b)
	})

	// Configure config variables
	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	cfg.RAG.SearchURL = "https://rag.test/search"
	cfg.RAG.SearchMethod = "GET"
	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
	}()

	mems, err := SearchThirdPartyRAG(ctx, "test-query")
	if err != nil {
		t.Fatalf("SearchThirdPartyRAG failed: %v", err)
	}

	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}

	if mems[0].ID != "mem-ext-99" || mems[0].Goal != "find database configuration" {
		t.Errorf("unexpected memory contents: %+v", mems[0])
	}
}

func TestSearchThirdPartyRAG_POST_Object(t *testing.T) {
	ctx := context.Background()

	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		if r.Method != "POST" {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		accept := r.Header.Get("Accept")
		if accept != "application/json, text/event-stream" {
			t.Errorf("expected Accept header 'application/json, text/event-stream', got %q", accept)
		}
		var reqBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to parse POST body: %v", err)
		}
		if reqBody["query"] != "post-query" {
			t.Errorf("expected POST query body to be 'post-query', got %q", reqBody["query"])
		}

		response := map[string]any{
			"results": []map[string]any{
				{
					"uuid":         "custom-uuid-1",
					"title":        "search database password",
					"content":      "found password in secrets.json",
					"final_answer": "secrets.json contains standard-username/standard-password",
				},
			},
		}

		b, _ := json.Marshal(response)
		return http.StatusOK, string(b)
	})

	// Configure config variables
	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	cfg.RAG.SearchURL = "https://rag.test/search"
	cfg.RAG.SearchMethod = "POST"
	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
	}()

	mems, err := SearchThirdPartyRAG(ctx, "post-query")
	if err != nil {
		t.Fatalf("SearchThirdPartyRAG failed: %v", err)
	}

	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}

	// Verify field mapping
	if mems[0].ID != "custom-uuid-1" {
		t.Errorf("expected ID 'custom-uuid-1' from mapping uuid, got %q", mems[0].ID)
	}
	if mems[0].Goal != "search database password" {
		t.Errorf("expected Goal 'search database password' from mapping title, got %q", mems[0].Goal)
	}
	if mems[0].KeyFindings != "found password in secrets.json" {
		t.Errorf("expected KeyFindings 'found password in secrets.json' from mapping content, got %q", mems[0].KeyFindings)
	}
	if mems[0].FinalAnswer != "secrets.json contains standard-username/standard-password" {
		t.Errorf("unexpected FinalAnswer: %q", mems[0].FinalAnswer)
	}
}

func TestSearchThirdPartyRAG_HttpErrors(t *testing.T) {
	ctx := context.Background()

	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		return http.StatusInternalServerError, "internal database down"
	})

	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	cfg.RAG.SearchURL = "https://rag.test/search"
	defer func() { cfg.RAG.SearchURL = originalURL }()

	_, err := SearchThirdPartyRAG(ctx, "error-query")
	if err == nil {
		t.Fatal("expected search to fail on 500 status code")
	}

	if !os.IsNotExist(err) && !os.IsPermission(err) {
		t.Logf("Expected search error: %v", err)
	}
}

func TestSearchThirdPartyRAG_Authorization(t *testing.T) {
	ctx := context.Background()

	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token-123" {
			t.Errorf("expected Authorization header 'Bearer test-token-123', got %q", auth)
		}

		response := []map[string]any{
			{
				"id":           "mem-ext-auth",
				"goal":         "test auth",
				"key_findings": "auth verified",
				"final_answer": "success",
			},
		}
		b, _ := json.Marshal(response)
		return http.StatusOK, string(b)
	})

	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	originalAuth := cfg.RAG.Authorization

	cfg.RAG.SearchURL = "https://rag.test/search"
	cfg.RAG.SearchMethod = "GET"
	cfg.RAG.Authorization = "Bearer test-token-123"

	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
		cfg.RAG.Authorization = originalAuth
	}()

	mems, err := SearchThirdPartyRAG(ctx, "test-query")
	if err != nil {
		t.Fatalf("SearchThirdPartyRAG failed: %v", err)
	}

	if len(mems) != 1 || mems[0].ID != "mem-ext-auth" {
		t.Errorf("unexpected memories: %+v", mems)
	}
}

func TestSearchThirdPartyRAG_MCP_JSONRPC(t *testing.T) {
	ctx := context.Background()

	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		if r.Method != "POST" {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		var reqBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to parse JSON-RPC request: %v", err)
		}
		if reqBody["jsonrpc"] != "2.0" {
			t.Errorf("expected jsonrpc to be '2.0', got %v", reqBody["jsonrpc"])
		}
		if reqBody["method"] != "tools/call" {
			t.Errorf("expected method to be 'tools/call', got %v", reqBody["method"])
		}
		params, _ := reqBody["params"].(map[string]any)
		if params["name"] != "search" {
			t.Errorf("expected tool name 'search', got %v", params["name"])
		}
		args, _ := params["arguments"].(map[string]any)
		if args["query"] != "test-query" {
			t.Errorf("expected query 'test-query', got %v", args["query"])
		}

		// Mock a successful JSON-RPC 2.0 response returning text content
		response := map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": `[{"id":"mem-mcp-1","goal":"test mcp","key_findings":"mcp verified","final_answer":"success"}]`,
					},
				},
				"isError": false,
			},
			"id": 1,
		}
		b, _ := json.Marshal(response)
		return http.StatusOK, string(b)
	})

	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	originalToolName := cfg.RAG.ToolName

	cfg.RAG.SearchURL = "https://rag.test/search/tools/call"
	cfg.RAG.SearchMethod = "POST" // will trigger isJSONRPC auto-detection due to URL pattern
	cfg.RAG.ToolName = "search"

	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
		cfg.RAG.ToolName = originalToolName
	}()

	mems, err := SearchThirdPartyRAG(ctx, "test-query")
	if err != nil {
		t.Fatalf("SearchThirdPartyRAG failed: %v", err)
	}

	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}

	if mems[0].ID != "mem-mcp-1" || mems[0].Goal != "test mcp" || mems[0].KeyFindings != "mcp verified" {
		t.Errorf("unexpected memories: %+v", mems)
	}
}

func TestSearchThirdPartyRAG_SelfHealingRetry(t *testing.T) {
	ctx := context.Background()

	attempts := 0
	withRAGHTTPClient(t, func(r *http.Request) (int, string) {
		attempts++
		if r.Method == "GET" {
			// Mock the SSE connection handshake response
			return http.StatusOK, "event: endpoint\ndata: https://rag.test/search/message\n\n"
		}

		// POST requests
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			t.Errorf("expected Accept header to contain both 'application/json' and 'text/event-stream', got %q", accept)
		}

		var reqBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to parse POST body: %v", err)
		}

		// If it's a standard POST request (not JSON-RPC 2.0), reject it to trigger retry
		if reqBody["jsonrpc"] != "2.0" {
			if reqBody["query"] != "retry-query" {
				t.Errorf("expected query 'retry-query', got %q", reqBody["query"])
			}
			return http.StatusBadRequest, `malformed payload: invalid message version tag ""; expected "2.0"`
		}

		// Handle JSON-RPC methods
		methodVal := reqBody["method"].(string)
		if methodVal == "initialize" {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
				},
				"id": reqBody["id"],
			}
			b, _ := json.Marshal(resp)
			return http.StatusOK, string(b)
		} else if methodVal == "notifications/initialized" {
			return http.StatusOK, ""
		} else if methodVal == "tools/call" {
			params, _ := reqBody["params"].(map[string]any)
			if params["name"] != "search" {
				t.Errorf("expected tool name 'search', got %v", params["name"])
			}
			response := map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{
							"type": "text",
							"text": `[{"id":"mem-retry-1","goal":"retry goal","key_findings":"healed","final_answer":"success"}]`,
						},
					},
					"isError": false,
				},
				"id": reqBody["id"],
			}
			b, _ := json.Marshal(response)
			return http.StatusOK, string(b)
		}

		return http.StatusBadRequest, "unknown method"
	})

	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	originalToolName := cfg.RAG.ToolName

	cfg.RAG.SearchURL = "https://rag.test/search/message" // does NOT contain "/tools/call"
	cfg.RAG.SearchMethod = "POST"
	cfg.RAG.ToolName = "search"

	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
		cfg.RAG.ToolName = originalToolName
	}()

	mems, err := SearchThirdPartyRAG(ctx, "retry-query")
	if err != nil {
		t.Fatalf("SearchThirdPartyRAG failed: %v", err)
	}

	// Attempts should be 5:
	// 1. Initial direct POST (failed with 400)
	// 2. SSE handshake GET
	// 3. POST initialize
	// 4. POST notifications/initialized
	// 5. POST tools/call
	if attempts != 5 {
		t.Errorf("expected exactly 5 attempts, got %d", attempts)
	}

	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}

	if mems[0].ID != "mem-retry-1" || mems[0].KeyFindings != "healed" {
		t.Errorf("unexpected memories: %+v", mems)
	}
}



