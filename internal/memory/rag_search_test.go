package memory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/memory"
)

func TestSearchThirdPartyRAG_GET(t *testing.T) {
	ctx := context.Background()

	// Mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Configure config variables
	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	cfg.RAG.SearchURL = server.URL
	cfg.RAG.SearchMethod = "GET"
	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
	}()

	mems, err := memory.SearchThirdPartyRAG(ctx, "test-query")
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

	// Mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					"uuid":        "custom-uuid-1",
					"title":       "search database password",
					"content":     "found password in secrets.json",
					"final_answer": "secrets.json contains standard-username/standard-password",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Configure config variables
	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	originalMethod := cfg.RAG.SearchMethod
	cfg.RAG.SearchURL = server.URL
	cfg.RAG.SearchMethod = "POST"
	defer func() {
		cfg.RAG.SearchURL = originalURL
		cfg.RAG.SearchMethod = originalMethod
	}()

	mems, err := memory.SearchThirdPartyRAG(ctx, "post-query")
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal database down"))
	}))
	defer server.Close()

	cfg := config.Get()
	originalURL := cfg.RAG.SearchURL
	cfg.RAG.SearchURL = server.URL
	defer func() { cfg.RAG.SearchURL = originalURL }()

	_, err := memory.SearchThirdPartyRAG(ctx, "error-query")
	if err == nil {
		t.Fatal("expected search to fail on 500 status code")
	}

	if !os.IsNotExist(err) && !os.IsPermission(err) {
		t.Logf("Expected search error: %v", err)
	}
}
