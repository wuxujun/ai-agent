package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
)

func TestWikiRuntimeStreamableHTTPEndToEnd(t *testing.T) {
	const sessionID = "wiki-test-session"
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			if request.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("close session header = %q", request.Header.Get("Mcp-Session-Id"))
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		var payload struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode MCP request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods = append(methods, payload.Method)
		mu.Unlock()
		if payload.Method != "initialize" {
			if request.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("%s session header = %q", payload.Method, request.Header.Get("Mcp-Session-Id"))
			}
			if request.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
				t.Errorf("%s protocol header = %q", payload.Method, request.Header.Get("MCP-Protocol-Version"))
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		switch payload.Method {
		case "initialize":
			writer.Header().Set("Mcp-Session-Id", sessionID)
			writeWikiRPCResult(t, writer, payload.ID, map[string]any{"protocolVersion": "2025-11-25"})
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeWikiRPCResult(t, writer, payload.ID, map[string]any{"tools": []any{
				map[string]any{"name": "wiki_search", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string"}, "top_k": map[string]any{"type": "integer"}, "wiki": map[string]any{"type": "string"},
				}}},
				map[string]any{"name": "wiki_content_read", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"path": map[string]any{"type": "string"}, "wiki": map[string]any{"type": "string"},
				}}},
			}})
		case "tools/call":
			switch payload.Params.Name {
			case "wiki_search":
				if payload.Params.Arguments["wiki"] != "engineering" {
					t.Errorf("wiki_search space = %v", payload.Params.Arguments["wiki"])
				}
				writeWikiRPCResult(t, writer, payload.ID, map[string]any{"structuredContent": map[string]any{"results": []any{
					map[string]any{"slug": "architecture/overview", "uri": "wiki://engineering/architecture/overview", "title": "Architecture", "excerpt": "System overview", "score": 0.97},
				}}})
			case "wiki_content_read":
				if payload.Params.Arguments["path"] != "architecture/overview" {
					t.Errorf("wiki_content_read path = %v", payload.Params.Arguments["path"])
				}
				writeWikiRPCResult(t, writer, payload.ID, map[string]any{"structuredContent": map[string]any{"content": "# Architecture\n\nThe orchestrator dispatches read-only tools."}})
			default:
				t.Errorf("unexpected remote tool %q", payload.Params.Name)
				writer.WriteHeader(http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected MCP method %q", payload.Method)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Wiki.DefaultSpace = "engineering"
		cfg.Wiki.SearchTopK = 5
		cfg.Wiki.FetchMaxItems = 3
		cfg.Wiki.FetchMaxBytes = 4096
	}))
	cfg := &config.Config{}
	cfg.Wiki.URL = server.URL
	cfg.Wiki.Required = true
	cfg.Wiki.AllowPrivateNetwork = true
	cfg.Wiki.TimeoutSeconds = 5
	registry := tools.NewRegistry()
	runtime, err := buildWikiRuntime(t.Context(), cfg, registry)
	if err != nil {
		t.Fatalf("build Wiki runtime: %v", err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close Wiki runtime: %v", err)
		}
	}()

	ctx := tools.WithRetrievalExecutionContext(t.Context(), "task-e2e", "default")
	search, _ := registry.Get("wiki_search")
	searchResult, err := search.Execute(ctx, "", map[string]any{"query": "architecture", "top_k": 3})
	if err != nil {
		t.Fatalf("wiki_search: %v", err)
	}
	var candidates struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(searchResult.Observation), &candidates); err != nil || len(candidates.Results) != 1 {
		t.Fatalf("search observation = %q, err=%v", searchResult.Observation, err)
	}
	fetch, _ := registry.Get("wiki_fetch")
	fetchResult, err := fetch.Execute(ctx, "", map[string]any{"ids": []string{candidates.Results[0].ID}})
	if err != nil {
		t.Fatalf("wiki_fetch: %v", err)
	}
	if len(fetchResult.Evidence) != 1 || fetchResult.Evidence[0].Path != "wiki://engineering/architecture/overview" {
		t.Fatalf("evidence = %+v", fetchResult.Evidence)
	}
	if !strings.Contains(fetchResult.Evidence[0].Lines[0], "orchestrator dispatches") {
		t.Fatalf("evidence content = %q", fetchResult.Evidence[0].Lines)
	}

	mu.Lock()
	gotMethods := strings.Join(methods, ",")
	mu.Unlock()
	if gotMethods != "initialize,notifications/initialized,tools/list,tools/call,tools/call" {
		t.Fatalf("MCP methods = %s", gotMethods)
	}
}

func writeWikiRPCResult(t *testing.T, writer http.ResponseWriter, id json.RawMessage, result any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Errorf("encode MCP response: %v", err)
	}
}
