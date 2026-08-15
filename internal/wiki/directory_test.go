package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryClientSearchAndReadRepositoryLayout(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(filepath.Join(wikiRoot, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wikiRoot, "comparisons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "concepts", "agents.md"), []byte("# Agents\n\nAgent orchestration and agent routing."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "comparisons", "models.md"), []byte("# Models\n\nModel comparison."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "agent routing", 3, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Slug != "concepts/agents" || documents[0].URI != "wiki://engineering/concepts/agents" {
		t.Fatalf("documents = %+v", documents)
	}
	page, err := client.Read(t.Context(), documents[0], "engineering")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Content, "Agent orchestration") {
		t.Fatalf("page content = %q", page.Content)
	}
}

func TestDirectoryClientRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(t.Context(), Document{Slug: "../AGENTS"}, "local"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("traversal error = %v", err)
	}
}
