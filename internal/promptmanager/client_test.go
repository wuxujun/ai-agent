package promptmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

func TestPromptManagerGet(t *testing.T) {
	// Setup mock Langfuse server
	called := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		// Verify Basic Auth
		username, password, ok := r.BasicAuth()
		if !ok || username != "pk-test" || password != "sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Verify query parameter
		if r.URL.Query().Get("label") != "production" {
			t.Errorf("expected label=production, got %q", r.URL.RawQuery)
		}
		// Return response
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   "test_prompt",
			"prompt": "Dynamic prompt content from Langfuse",
		})
	}))
	defer server.Close()

	// Update config via environment variables
	os.Setenv("AI_AGENT_LANGFUSE_ENABLED", "true")
	os.Setenv("AI_AGENT_LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("AI_AGENT_LANGFUSE_SECRET_KEY", "sk-test")
	os.Setenv("AI_AGENT_LANGFUSE_HOST", server.URL)
	defer func() {
		os.Unsetenv("AI_AGENT_LANGFUSE_ENABLED")
		os.Unsetenv("AI_AGENT_LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("AI_AGENT_LANGFUSE_SECRET_KEY")
		os.Unsetenv("AI_AGENT_LANGFUSE_HOST")
	}()

	// Reload config to pick up env vars
	config.Reload()

	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt) // clear cache

	ctx := context.Background()
	fallback := "Local fallback prompt content"

	// 1. First get: cache miss, fetch from server
	p1 := manager.Get(ctx, "test_prompt", fallback)
	if p1 != "Dynamic prompt content from Langfuse" {
		t.Errorf("expected dynamic content, got %q", p1)
	}
	if called != 1 {
		t.Errorf("expected 1 server call, got %d", called)
	}

	// 2. Second get: cache hit, no server call
	p2 := manager.Get(ctx, "test_prompt", fallback)
	if p2 != "Dynamic prompt content from Langfuse" {
		t.Errorf("expected dynamic content, got %q", p2)
	}
	if called != 1 {
		t.Errorf("expected 1 server call (cached), got %d", called)
	}

	// 3. Fallback scenario when server fails
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	os.Setenv("AI_AGENT_LANGFUSE_HOST", serverErr.URL)
	config.Reload()

	manager.cache = make(map[string]cachedPrompt) // clear cache again

	p3 := manager.Get(ctx, "failed_prompt", fallback)
	if p3 != fallback {
		t.Errorf("expected fallback content, got %q", p3)
	}
}

func TestPromptManagerDisabled(t *testing.T) {
	os.Setenv("AI_AGENT_LANGFUSE_ENABLED", "false")
	config.Reload()
	defer os.Unsetenv("AI_AGENT_LANGFUSE_ENABLED")

	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt) // clear cache

	fallback := "Fallback"
	p := manager.Get(context.Background(), "some_prompt", fallback)
	if p != fallback {
		t.Errorf("expected fallback when disabled, got %q", p)
	}
}

func TestRenderPromptContentSupportsChatPrompt(t *testing.T) {
	content, err := renderPromptContent(json.RawMessage(`[
		{"role":"system","content":"Follow the evidence."},
		{"role":"user","content":"Return JSON only."}
	]`))
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}
	expected := "system: Follow the evidence.\n\nuser: Return JSON only."
	if content != expected {
		t.Fatalf("content = %q; want %q", content, expected)
	}
}

func TestPromptManagerCacheIsScopedByHostAndKey(t *testing.T) {
	serverOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   "shared_prompt",
			"prompt": "prompt from server one",
		})
	}))
	defer serverOne.Close()

	serverTwo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   "shared_prompt",
			"prompt": "prompt from server two",
		})
	}))
	defer serverTwo.Close()

	os.Setenv("AI_AGENT_LANGFUSE_ENABLED", "true")
	os.Setenv("AI_AGENT_LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("AI_AGENT_LANGFUSE_SECRET_KEY", "sk-test")
	os.Setenv("AI_AGENT_LANGFUSE_HOST", serverOne.URL)
	defer func() {
		os.Unsetenv("AI_AGENT_LANGFUSE_ENABLED")
		os.Unsetenv("AI_AGENT_LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("AI_AGENT_LANGFUSE_SECRET_KEY")
		os.Unsetenv("AI_AGENT_LANGFUSE_HOST")
		config.Reload()
	}()
	config.Reload()

	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt)

	ctx := context.Background()
	first := manager.Get(ctx, "shared_prompt", "fallback")
	if first != "prompt from server one" {
		t.Fatalf("first prompt = %q", first)
	}

	os.Setenv("AI_AGENT_LANGFUSE_HOST", serverTwo.URL)
	config.Reload()

	second := manager.Get(ctx, "shared_prompt", "fallback")
	if second != "prompt from server two" {
		t.Fatalf("second prompt = %q; want server two prompt", second)
	}
}

func TestBuildPromptURLEscapesPromptNameAsSinglePathSegment(t *testing.T) {
	endpoint, err := buildPromptURL("https://cloud.langfuse.com/base/", "folder/a b/中文")
	if err != nil {
		t.Fatalf("build prompt URL: %v", err)
	}
	expected := "https://cloud.langfuse.com/base/api/public/v2/prompts/folder%2Fa%20b%2F%E4%B8%AD%E6%96%87?label=production"
	if endpoint != expected {
		t.Fatalf("endpoint = %q; want %q", endpoint, expected)
	}
}
