package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type fakeWikiClient struct {
	initialized bool
	closed      bool
}

func TestBuildWikiRuntimeFromLocalDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "entities"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "entities", "router.md"), []byte("# Router\n\nRoutes tasks."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Wiki.Directory = root
	cfg.Wiki.Required = true
	registry := tools.NewRegistry()
	runtime, err := buildWikiRuntime(t.Context(), cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, ok := registry.Get("wiki_search"); !ok {
		t.Fatal("local wiki_search was not registered")
	}
	if _, ok := registry.Get("wiki_fetch"); !ok {
		t.Fatal("local wiki_fetch was not registered")
	}
}

func (f *fakeWikiClient) Initialize(context.Context) error { f.initialized = true; return nil }
func (f *fakeWikiClient) Probe(context.Context) error      { return nil }
func (f *fakeWikiClient) Close(context.Context) error      { f.closed = true; return nil }
func (f *fakeWikiClient) Search(context.Context, string, int, string) ([]wiki.Document, error) {
	return nil, nil
}
func (f *fakeWikiClient) Read(context.Context, wiki.Document, string) (wiki.Document, error) {
	return wiki.Document{}, nil
}

func TestBuildWikiRuntimeRegistersOnlyReadTools(t *testing.T) {
	cfg := &config.Config{}
	cfg.Wiki.URL = "https://wiki.example/mcp"
	cfg.Wiki.Required = true
	registry := tools.NewRegistry()
	client := &fakeWikiClient{}
	runtime, err := buildWikiRuntimeWithFactory(t.Context(), cfg, registry, func(got wiki.Config) (wikiClient, error) {
		if got.URL != cfg.Wiki.URL {
			t.Fatalf("wiki URL = %q", got.URL)
		}
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !client.initialized {
		t.Fatal("wiki client was not initialized")
	}
	for _, name := range []string{"wiki_search", "wiki_fetch"} {
		tool, ok := registry.Get(name)
		if !ok || tool.RiskLevel() != "low" {
			t.Fatalf("tool %q missing or not low-risk", name)
		}
	}
	for _, name := range []string{"wiki_content_write", "wiki_ingest", "wiki_content_commit"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("mutating Wiki tool %q was registered", name)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if !client.closed {
		t.Fatal("wiki client was not closed")
	}
}

func TestBuildWikiRuntimeSkipsUnconfiguredWiki(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := buildWikiRuntimeWithFactory(t.Context(), &config.Config{}, registry, func(wiki.Config) (wikiClient, error) {
		t.Fatal("factory should not be called")
		return nil, nil
	})
	if err != nil || runtime == nil || len(registry.Names()) != 0 {
		t.Fatalf("runtime=%#v err=%v tools=%v", runtime, err, registry.Names())
	}
}
