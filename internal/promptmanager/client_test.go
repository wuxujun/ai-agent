package promptmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
	os.Setenv("LANGFUSE_ENABLED", "true")
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	os.Setenv("LANGFUSE_BASE_URL", server.URL)
	defer func() {
		os.Unsetenv("LANGFUSE_ENABLED")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
		os.Unsetenv("LANGFUSE_BASE_URL")
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
	if _, err := manager.ResolveStrict(ctx, "test_prompt", Selector{}); err == nil || !strings.Contains(err.Error(), "no positive version") {
		t.Fatalf("expected strict resolution to require response version, got %v", err)
	}

	// 3. Fallback scenario when server fails
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	os.Setenv("LANGFUSE_BASE_URL", serverErr.URL)
	config.Reload()

	manager.cache = make(map[string]cachedPrompt) // clear cache again

	p3 := manager.Get(ctx, "failed_prompt", fallback)
	if p3 != fallback {
		t.Errorf("expected fallback content, got %q", p3)
	}
}

func TestPromptManagerDisabled(t *testing.T) {
	os.Setenv("LANGFUSE_ENABLED", "false")
	config.Reload()
	defer os.Unsetenv("LANGFUSE_ENABLED")

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

	os.Setenv("LANGFUSE_ENABLED", "true")
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	os.Setenv("LANGFUSE_BASE_URL", serverOne.URL)
	defer func() {
		os.Unsetenv("LANGFUSE_ENABLED")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
		os.Unsetenv("LANGFUSE_BASE_URL")
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

	os.Setenv("LANGFUSE_BASE_URL", serverTwo.URL)
	config.Reload()

	second := manager.Get(ctx, "shared_prompt", "fallback")
	if second != "prompt from server two" {
		t.Fatalf("second prompt = %q; want server two prompt", second)
	}
}

func TestBuildPromptURLEscapesPromptNameAsSinglePathSegment(t *testing.T) {
	endpoint, err := buildPromptURL("https://cloud.langfuse.com/base/", "folder/a b/中文", Selector{})
	if err != nil {
		t.Fatalf("build prompt URL: %v", err)
	}
	expected := "https://cloud.langfuse.com/base/api/public/v2/prompts/folder%2Fa%20b%2F%E4%B8%AD%E6%96%87?label=production"
	if endpoint != expected {
		t.Fatalf("endpoint = %q; want %q", endpoint, expected)
	}
}

func TestPromptManagerSelectorScopesCacheAndRequest(t *testing.T) {
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selector := r.URL.RawQuery
		calls[selector]++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":    "critic",
			"version": 7,
			"labels":  []string{"latest"},
			"prompt":  "prompt for " + selector,
		})
	}))
	defer server.Close()

	os.Setenv("LANGFUSE_ENABLED", "true")
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	os.Setenv("LANGFUSE_BASE_URL", server.URL)
	defer func() {
		os.Unsetenv("LANGFUSE_ENABLED")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
		os.Unsetenv("LANGFUSE_BASE_URL")
		config.Reload()
	}()
	config.Reload()

	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt)
	ctx := context.Background()
	production := manager.GetWithSelector(ctx, "critic", Selector{Label: "production"}, "fallback")
	latest := manager.GetWithSelector(ctx, "critic", Selector{Label: "latest"}, "fallback")
	version := manager.GetWithSelector(ctx, "critic", Selector{Version: 7}, "fallback")
	versionAgain := manager.GetWithSelector(ctx, "critic", Selector{Version: 7}, "fallback")

	if production != "prompt for label=production" || latest != "prompt for label=latest" || version != "prompt for version=7" || versionAgain != version {
		t.Fatalf("production=%q latest=%q version=%q cached=%q", production, latest, version, versionAgain)
	}
	if calls["label=production"] != 1 || calls["label=latest"] != 1 || calls["version=7"] != 1 {
		t.Fatalf("calls=%v", calls)
	}

	versionSelector := Selector{Version: 7}
	cacheKey := promptCacheKey(server.URL, "pk-test", "critic", versionSelector)
	manager.cache[cacheKey] = cachedPrompt{resolved: fallbackPrompt("critic", versionSelector, "fallback"), expiredAt: time.Now().Add(time.Minute)}
	strict, err := manager.ResolveStrict(ctx, "critic", versionSelector)
	if err != nil || strict.Content != "prompt for version=7" || strict.Version != 7 || len(strict.Labels) != 1 || strict.Labels[0] != "latest" || strict.Source != "langfuse" || calls["version=7"] != 2 {
		t.Fatalf("strict=%+v err=%v calls=%v", strict, err, calls)
	}
}

func TestSelectorValidationAndVersionURL(t *testing.T) {
	if _, err := (Selector{Label: "latest", Version: 2}).Normalize(); err == nil {
		t.Fatal("expected label and version conflict")
	}
	if _, err := (Selector{Version: -1}).Normalize(); err == nil {
		t.Fatal("expected negative version rejection")
	}
	endpoint, err := buildPromptURL("https://cloud.langfuse.com", "critic", Selector{Version: 12})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://cloud.langfuse.com/api/public/v2/prompts/critic?version=12" {
		t.Fatalf("endpoint=%q", endpoint)
	}
}

func TestPromptManagerGetStrictRequiresLangfuse(t *testing.T) {
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "false")
	config.Reload()
	if _, err := GetManager().GetStrict(context.Background(), "critic", Selector{}); err == nil {
		t.Fatal("expected disabled Langfuse to fail strict prompt resolution")
	}
}

func TestResolveRecordsMetadataWithoutPromptContent(t *testing.T) {
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "false")
	config.Reload()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "resolve")
	resolved := GetManager().Resolve(ctx, "teams/test/critic", Selector{Label: "production"}, "sensitive prompt body")
	span.End()
	if resolved.Source != "fallback" || resolved.Content != "sensitive prompt body" || resolved.Selector.Label != "production" {
		t.Fatalf("resolved=%+v", resolved)
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sensitive prompt body") {
		t.Fatalf("serialized metadata leaked prompt content: %s", raw)
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans=%d, want parent and prompt resolution child", len(spans))
	}
	parent := findEndedSpan(t, spans, "resolve")
	if len(parent.Events()) != 1 {
		t.Fatalf("parent events=%d, want 1", len(parent.Events()))
	}
	event := parent.Events()[0]
	if event.Name != "agent.prompt.resolved" {
		t.Fatalf("event=%+v", event)
	}
	for _, attr := range event.Attributes {
		if strings.Contains(attr.Value.Emit(), "sensitive prompt body") {
			t.Fatalf("span attribute leaked prompt content: %+v", attr)
		}
	}
	resolution := findEndedSpan(t, spans, promptResolutionSpanName)
	resolutionAttributes := spanAttributes(resolution)
	if resolutionAttributes["agent.prompt.name"] != "teams/test/critic" ||
		resolutionAttributes["agent.prompt.source"] != "fallback" ||
		resolutionAttributes["agent.prompt.outcome"] != "disabled_or_unconfigured" {
		t.Fatalf("resolution span attributes=%v", resolutionAttributes)
	}
	for _, value := range resolutionAttributes {
		if strings.Contains(value, "sensitive prompt body") {
			t.Fatalf("resolution span leaked prompt content: %v", resolutionAttributes)
		}
	}
}

func TestResolutionSpanIncludesActualVersion(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "resolve")
	recordPromptResolution(ctx, ResolvedPrompt{
		Name: "teams/test/critic", Version: 23, Labels: []string{"production"},
		Selector: Selector{Label: "production"}, Source: "langfuse", Content: "do not record",
	}, "fetched")
	span.End()
	spans := recorder.Ended()
	event := findEndedSpan(t, spans, "resolve").Events()[0]
	attributes := make(map[string]string, len(event.Attributes))
	for _, attr := range event.Attributes {
		attributes[string(attr.Key)] = attr.Value.Emit()
	}
	if attributes["agent.prompt.version"] != "23" || attributes["agent.prompt.source"] != "langfuse" || attributes["agent.prompt.selector"] != "label:production" {
		t.Fatalf("attributes=%v", attributes)
	}
	for _, value := range attributes {
		if strings.Contains(value, "do not record") {
			t.Fatalf("attributes leaked prompt content: %v", attributes)
		}
	}
	resolutionAttributes := spanAttributes(findEndedSpan(t, spans, promptResolutionSpanName))
	if resolutionAttributes["agent.prompt.version"] != "23" ||
		resolutionAttributes["agent.prompt.source"] != "langfuse" ||
		resolutionAttributes["agent.prompt.selector"] != "label:production" {
		t.Fatalf("resolution span attributes=%v", resolutionAttributes)
	}
}

func TestResolutionFailureCreatesVisibleErrorSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "resolve")
	recordPromptResolutionError(ctx, "teams/test/critic", Selector{Label: "production"}, "fetch_error")
	span.End()

	spans := recorder.Ended()
	parent := findEndedSpan(t, spans, "resolve")
	if len(parent.Events()) != 1 || parent.Events()[0].Name != "agent.prompt.resolve_failed" {
		t.Fatalf("parent events=%+v", parent.Events())
	}
	resolution := findEndedSpan(t, spans, promptResolutionSpanName)
	if resolution.Status().Code != codes.Error || resolution.Status().Description != "fetch_error" {
		t.Fatalf("resolution status=%+v", resolution.Status())
	}
	attributes := spanAttributes(resolution)
	if attributes["agent.prompt.name"] != "teams/test/critic" ||
		attributes["agent.prompt.outcome"] != "fetch_error" {
		t.Fatalf("resolution span attributes=%v", attributes)
	}
}

func findEndedSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found in %d ended spans", name, len(spans))
	return nil
}

func spanAttributes(span sdktrace.ReadOnlySpan) map[string]string {
	attributes := make(map[string]string, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attributes[string(attr.Key)] = attr.Value.Emit()
	}
	return attributes
}
