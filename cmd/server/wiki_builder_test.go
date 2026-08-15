package main

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type fakeWikiClient struct {
	initialized bool
	closed      bool
}

func (f *fakeWikiClient) Initialize(context.Context) error { f.initialized = true; return nil }
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
