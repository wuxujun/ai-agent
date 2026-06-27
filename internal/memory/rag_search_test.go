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
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
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

