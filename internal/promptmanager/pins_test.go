package promptmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

func TestVersionPinRegistryPinsOnceAndRejectsConflict(t *testing.T) {
	var callbacks atomic.Int32
	registry, err := NewVersionPinRegistry(nil, func(VersionPin) { callbacks.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	pin := VersionPin{Name: "critic", Version: 7, Selector: Selector{Label: "production"}, Labels: []string{"production"}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := registry.Pin(pin); err != nil {
				t.Errorf("pin: %v", err)
			}
		}()
	}
	wg.Wait()
	if callbacks.Load() != 1 {
		t.Fatalf("callbacks=%d", callbacks.Load())
	}
	got, ok := registry.Get("critic")
	if !ok || got.Version != 7 || got.Selector.Label != "production" {
		t.Fatalf("pin=%+v ok=%t", got, ok)
	}
	if err := registry.Pin(VersionPin{Name: "critic", Version: 8}); err == nil {
		t.Fatal("expected conflicting version rejection")
	}
}

func TestResolvePinnedChangesLabelToVersionAfterFirstResolution(t *testing.T) {
	queries := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries[r.URL.RawQuery]++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "critic", "version": 7, "labels": []string{"production"}, "prompt": "pinned prompt",
		})
	}))
	defer server.Close()
	configurePromptTestLangfuse(t, server.URL)
	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt)
	var callbacks atomic.Int32
	registry, err := NewVersionPinRegistry(nil, func(VersionPin) { callbacks.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithVersionPinRegistry(context.Background(), registry)
	first, err := manager.ResolvePinned(ctx, "critic", Selector{Label: "production"}, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.ResolvePinned(ctx, "critic", Selector{Label: "production"}, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 7 || second.Version != 7 || callbacks.Load() != 1 {
		t.Fatalf("first=%+v second=%+v callbacks=%d", first, second, callbacks.Load())
	}
	if queries["label=production"] != 1 || queries["version=7"] != 1 {
		t.Fatalf("queries=%v", queries)
	}
}

func TestResolvePinnedFailsClosedWhenPinnedVersionIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	configurePromptTestLangfuse(t, server.URL)
	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt)
	registry, err := NewVersionPinRegistry([]VersionPin{{Name: "critic", Version: 7, Selector: Selector{Label: "production"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithVersionPinRegistry(context.Background(), registry)
	resolved, err := manager.ResolvePinned(ctx, "critic", Selector{Label: "production"}, "must not be used")
	if err == nil || resolved.Content != "" {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
}

func TestResolvePinnedRejectsWrongReturnedVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "critic", "version": 8, "prompt": "wrong version",
		})
	}))
	defer server.Close()
	configurePromptTestLangfuse(t, server.URL)
	manager := GetManager()
	manager.cache = make(map[string]cachedPrompt)
	registry, err := NewVersionPinRegistry([]VersionPin{{Name: "critic", Version: 7, Selector: Selector{Label: "production"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithVersionPinRegistry(context.Background(), registry)
	if _, err := manager.ResolvePinned(ctx, "critic", Selector{Label: "production"}, "fallback"); err == nil {
		t.Fatal("expected returned version mismatch to fail")
	}
}

func configurePromptTestLangfuse(t *testing.T, host string) {
	t.Helper()
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "true")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	t.Setenv("LANGFUSE_BASE_URL", host)
	config.Reload()
}
