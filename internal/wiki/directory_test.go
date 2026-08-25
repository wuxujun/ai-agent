package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	status := client.Status()
	if status.Backend != "directory" || status.SearchMode != SearchModeLegacy || status.PageCount != 2 || status.IndexVersion == "" || status.IndexBuiltAtUnixMS == 0 || status.LastRefreshSuccessUnixMS == 0 || status.ServingStaleSnapshot {
		t.Fatalf("directory status = %+v", status)
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

func TestDirectoryClientRanksChineseTitleAndPhraseAboveBodyFrequency(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(filepath.Join(wikiRoot, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"concepts/pbl-new-york.md":  "# PBL 历史旅行指南：纽约篇\n\n面向学生的项目制历史写作课程。",
		"concepts/keyword-notes.md": "# 关键词笔记\n\nPBL PBL PBL。历史旅行指南可用于课程检索。",
	}
	for name, content := range pages {
		if err := os.WriteFile(filepath.Join(wikiRoot, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "PBL 历史旅行指南", 2, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0].Slug != "concepts/pbl-new-york" {
		t.Fatalf("ranked documents = %+v", documents)
	}
	if documents[0].Score <= documents[1].Score {
		t.Fatalf("title score %.0f <= body score %.0f", documents[0].Score, documents[1].Score)
	}
}

func TestDirectoryClientSearchExplainPreservesRankingAndDetails(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	for _, category := range []string{"concepts", "sources"} {
		if err := os.MkdirAll(filepath.Join(wikiRoot, category), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "concepts", "guide.md"), []byte("# Historical Travel Guide\n\n[Course source](../sources/course.md)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "sources", "course.md"), []byte("# Course Source\n\nOriginal material.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	want, err := client.Search(t.Context(), "Historical Travel Guide", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	documents, explanations, err := client.SearchWithExplain(t.Context(), "Historical Travel Guide", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != len(want) || len(explanations) != len(documents) {
		t.Fatalf("documents=%+v want=%+v explanations=%+v", documents, want, explanations)
	}
	for index := range documents {
		if documents[index].URI != want[index].URI || documents[index].Score != want[index].Score || explanations[index].URI != documents[index].URI || explanations[index].FinalScore != documents[index].Score {
			t.Fatalf("index=%d document=%+v want=%+v explanation=%+v", index, documents[index], want[index], explanations[index])
		}
		if explanations[index].IndexVersion == "" || explanations[index].NavigationMultiplier != 1 {
			t.Fatalf("explanation=%+v", explanations[index])
		}
	}
	if explanations[0].BaseScore == 0 || explanations[0].FieldScores["title"] == 0 || len(explanations[0].PhraseMatches) == 0 || len(explanations[0].MatchedTerms) != 3 {
		t.Fatalf("root explanation=%+v", explanations[0])
	}
	if len(explanations) < 2 || explanations[1].LinkBoost == 0 || explanations[1].BaseScore != 0 {
		t.Fatalf("linked explanation=%+v", explanations)
	}
}

func TestDirectoryClientIndexRefreshesAfterFileChange(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(filepath.Join(wikiRoot, "entities"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wikiRoot, "entities", "mentor.md")
	if err := os.WriteFile(path, []byte("# Mentor\n\nInitial biography."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if documents, err := client.Search(t.Context(), "Vanessa", 5, "local"); err != nil || len(documents) != 0 {
		t.Fatalf("initial search = %+v, err=%v", documents, err)
	}
	if err := os.WriteFile(path, []byte("# Vanessa Ruales\n\nUpdated mentor biography."), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "Vanessa", 5, "local")
	if err != nil || len(documents) != 1 || documents[0].Title != "Vanessa Ruales" {
		t.Fatalf("refreshed search = %+v, err=%v", documents, err)
	}
}

func TestDirectoryClientAutomaticRefreshPublishesNewSnapshot(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wikiRoot, "guide.md")
	if err := os.WriteFile(path, []byte("# Initial Guide\n\nOld content."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25), WithRefreshInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	initial := client.indexSnapshot.Load()
	if initial == nil {
		t.Fatal("initial snapshot was not published")
	}
	if err := os.WriteFile(path, []byte("# Updated Guide\n\nAtomic snapshot content."), 0o600); err != nil {
		t.Fatal(err)
	}
	client.nextRefresh.Store(time.Now().Add(-time.Second).UnixNano())
	documents, err := client.Search(t.Context(), "Atomic snapshot", 5, "local")
	if err != nil || len(documents) != 1 || documents[0].Title != "Updated Guide" {
		t.Fatalf("refreshed documents=%+v err=%v", documents, err)
	}
	updated := client.indexSnapshot.Load()
	if updated == nil || updated == initial || updated.version == initial.version {
		t.Fatalf("snapshot was not atomically replaced: initial=%p updated=%p", initial, updated)
	}
}

func TestDirectoryClientZeroRefreshIntervalRequiresProbe(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wikiRoot, "guide.md")
	if err := os.WriteFile(path, []byte("# Initial Guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root, WithRefreshInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Updated Guide\n\nProbe only phrase."), 0o600); err != nil {
		t.Fatal(err)
	}
	if documents, err := client.Search(t.Context(), "Probe only phrase", 5, "local"); err != nil || len(documents) != 0 {
		t.Fatalf("unprobed documents=%+v err=%v", documents, err)
	}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if documents, err := client.Search(t.Context(), "Probe only phrase", 5, "local"); err != nil || len(documents) != 1 {
		t.Fatalf("probed documents=%+v err=%v", documents, err)
	}
}

func TestDirectoryClientRefreshFailureKeepsLastCompleteSnapshot(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "stable.md"), []byte("# Stable Guide\n\nKnown good content."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root, WithRefreshInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	initial := client.indexSnapshot.Load()
	if err := os.WriteFile(filepath.Join(wikiRoot, "oversized.md"), make([]byte, maxDirectoryPageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	client.nextRefresh.Store(time.Now().Add(-time.Second).UnixNano())
	documents, err := client.Search(t.Context(), "Stable Guide", 5, "local")
	if err != nil || len(documents) != 1 || documents[0].Slug != "concepts/stable" {
		t.Fatalf("stale snapshot documents=%+v err=%v", documents, err)
	}
	if client.indexSnapshot.Load() != initial {
		t.Fatal("failed refresh replaced the complete snapshot")
	}
	if err := client.Probe(t.Context()); err == nil {
		t.Fatal("Probe did not report the refresh failure")
	}
	status := client.Status()
	if !status.ServingStaleSnapshot || status.ConsecutiveRefreshFailures < 2 || status.PageCount != 1 || status.IndexVersion == "" {
		t.Fatalf("failed refresh status = %+v", status)
	}
}

func TestDirectoryClientConcurrentSearchesShareIndexSafely(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(filepath.Join(wikiRoot, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "sources", "course.md"), []byte("# PBL Course\n\nHistorical travel guide."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			documents, err := client.Search(t.Context(), "PBL course", 3, "local")
			if err != nil {
				errorsFound <- err
				return
			}
			if len(documents) != 1 {
				errorsFound <- fmt.Errorf("documents=%d", len(documents))
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func TestDirectoryClientAddsOneHopRelatedWikiPages(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	for _, category := range []string{"concepts", "entities", "sources"} {
		if err := os.MkdirAll(filepath.Join(wikiRoot, category), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pages := map[string]string{
		"concepts/pbl-guide.md":      "# PBL 历史旅行指南\n\n课程由 [Vanessa Ruales](../entities/vanessa-ruales.md) 指导。\n\n## 来源\n- [课程来源](../sources/course.md)",
		"entities/vanessa-ruales.md": "# Vanessa Ruales\n\n写作导师。",
		"sources/course.md":          "# 课程产品说明\n\n原始课程材料。",
		"index.md":                   "# Index\n\n[PBL](concepts/pbl-guide.md) [Vanessa](entities/vanessa-ruales.md)",
	}
	for name, content := range pages {
		if err := os.WriteFile(filepath.Join(wikiRoot, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "PBL 历史旅行指南", 4, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) < 3 || documents[0].Slug != "concepts/pbl-guide" {
		t.Fatalf("related search results = %+v", documents)
	}
	seen := make(map[string]bool)
	for _, document := range documents {
		seen[document.Slug] = true
	}
	for _, slug := range []string{"entities/vanessa-ruales", "sources/course"} {
		if !seen[slug] {
			t.Fatalf("linked page %q missing from %+v", slug, documents)
		}
	}
	if len(documents) > 1 && documents[1].Slug == "index" {
		t.Fatalf("navigation index outranked related content: %+v", documents)
	}
}

func TestDirectoryClientIndexesFrontmatterMetadata(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
title: "纽约历史写作项目"
summary: "面向中学生的二十世纪城市研究课程"
tags: [PBL, history]
aliases: ["Historical Travel Guide", "历史旅行指南"]
---
# 页面旧标题

正文没有查询使用的英文别名。
`
	if err := os.WriteFile(filepath.Join(wikiRoot, "project.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "Historical Travel Guide", 3, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Title != "纽约历史写作项目" || documents[0].Summary != "面向中学生的二十世纪城市研究课程" {
		t.Fatalf("frontmatter result = %+v", documents)
	}
}

func TestDirectoryClientMatchesMixedLanguageSpacingVariants(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "pbl-new-york.md"), []byte("# PBL 历史旅行指南：纽约篇\n\n项目制历史写作课程。"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"PBL历史旅行指南",
		"PBL-历史旅行指南",
		"PBL：历史旅行指南",
	} {
		documents, err := client.Search(t.Context(), query, 3, "local")
		if err != nil || len(documents) != 1 || documents[0].Slug != "concepts/pbl-new-york" {
			t.Fatalf("query %q documents = %+v, err=%v", query, documents, err)
		}
	}
}

func TestDirectoryClientReturnsQueryRelevantExcerpt(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "sources")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# Course Source\n\nGeneral introduction without the target.\n\n## Enrollment\n\n申请者须提交原创说明文写作样本，并与学术顾问沟通。"
	if err := os.WriteFile(filepath.Join(wikiRoot, "course.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "说明文写作样本", 3, "local")
	if err != nil || len(documents) != 1 {
		t.Fatalf("documents = %+v, err=%v", documents, err)
	}
	if !strings.Contains(documents[0].Excerpt, "说明文写作样本") || strings.Contains(documents[0].Excerpt, "General introduction") {
		t.Fatalf("query-relevant excerpt = %q", documents[0].Excerpt)
	}
}

func TestDirectoryClientMalformedFrontmatterFallsBackToMarkdown(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ntags: [broken\n---\n# Recoverable Page\n\nFallback keyword."
	if err := os.WriteFile(filepath.Join(wikiRoot, "recoverable.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "Fallback keyword", 3, "local")
	if err != nil || len(documents) != 1 {
		t.Fatalf("fallback documents = %+v, err=%v", documents, err)
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

func TestDirectoryClientReadRejectsNonMarkdownAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "private.json"), []byte(`{"secret":"not wiki content"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(t.Context(), Document{Slug: "private.json"}, "local"); err == nil || !strings.Contains(err.Error(), "Markdown") {
		t.Fatalf("non-Markdown read error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "oversized.md"), make([]byte, maxDirectoryPageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(t.Context(), Document{Slug: "oversized"}, "local"); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("oversized read error = %v", err)
	}
}

func TestDirectoryClientGraphReturnsBoundedIncomingAndOutgoingLinks(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"concepts", "entities", "sources"} {
		if err := os.MkdirAll(filepath.Join(root, "wiki", directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pages := map[string]string{
		"concepts/course.md":  "# Course\n\n[Teacher](../entities/teacher.md) and [Source](../sources/course.md).",
		"entities/teacher.md": "# Teacher\n\n[Course](../concepts/course.md).",
		"sources/course.md":   "# Source\n\nDetails.",
	}
	for path, content := range pages {
		if err := os.WriteFile(filepath.Join(root, "wiki", filepath.FromSlash(path)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	graph, err := client.Graph(t.Context(), Document{URI: "wiki://local/concepts/course"}, "local", 1, "both")
	if err != nil {
		t.Fatal(err)
	}
	if graph.RootURI != "wiki://local/concepts/course" || len(graph.Nodes) != 3 || len(graph.Edges) != 3 {
		t.Fatalf("graph = %+v", graph)
	}
	if _, err := client.Graph(t.Context(), Document{URI: "wiki://other/concepts/course"}, "local", 1, "both"); err == nil {
		t.Fatal("cross-space graph URI was accepted")
	}
}

func TestDirectoryClientGraphBoundsHubExpansionByRootRelevance(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	for _, category := range []string{"concepts", "entities", "sources"} {
		if err := os.MkdirAll(filepath.Join(wikiRoot, category), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rootContent := "# PBL Historical Travel Guide\n\n[Catalog](../sources/catalog.md) [Course](../sources/course.md)"
	if err := os.WriteFile(filepath.Join(wikiRoot, "concepts", "guide.md"), []byte(rootContent), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogLinks := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		slug := fmt.Sprintf("distractor-%02d", index)
		catalogLinks = append(catalogLinks, fmt.Sprintf("[%s](../concepts/%s.md)", slug, slug))
		if err := os.WriteFile(filepath.Join(wikiRoot, "concepts", slug+".md"), []byte("# Unrelated Topic\n\nGeneric catalog entry."), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "sources", "catalog.md"), []byte("# Catalog\n\n"+strings.Join(catalogLinks, " ")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "sources", "course.md"), []byte("# Course Source\n\n[Mentor](../entities/mentor.md)"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "entities", "mentor.md"), []byte("# Vanessa Mentor\n\nMentor for the PBL Historical Travel Guide."), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	graph, err := client.Graph(t.Context(), Document{URI: "wiki://local/concepts/guide"}, "local", 2, "outgoing")
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) > defaultDirectoryGraphMaxNodes {
		t.Fatalf("graph nodes=%d exceeds local bound", len(graph.Nodes))
	}
	foundMentor := false
	for _, node := range graph.Nodes {
		if node.Slug == "entities/mentor" {
			foundMentor = true
		}
	}
	if !foundMentor {
		t.Fatalf("root-relevant second-hop mentor missing from graph: %+v", graph.Nodes)
	}
	for _, edge := range graph.Edges {
		fromFound, toFound := false, false
		for _, node := range graph.Nodes {
			fromFound = fromFound || node.URI == edge.From
			toFound = toFound || node.URI == edge.To
		}
		if !fromFound || !toFound {
			t.Fatalf("edge references a pruned node: %+v", edge)
		}
	}
}

func TestDirectoryClientSuggestsDuplicatesMissingLinksAndRelatedPages(t *testing.T) {
	root := t.TempDir()
	pages := map[string]string{
		"concepts/pbl-new-york-guide.md":         "# PBL New York Guide\n\n[Teacher](../entities/teacher.md).",
		"concepts/pbl-new-york-guide-revised.md": "# PBL New York Guide Revised\n\nA second course guide.",
		"sources/pbl-new-york-course-details.md": "# PBL New York Course Details\n\nPBL New York Guide curriculum details.",
		"entities/teacher.md":                    "# Teacher\n\nCourse mentor.",
	}
	for path, content := range pages {
		fullPath := filepath.Join(root, "wiki", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := NewDirectory(root)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := client.Suggest(t.Context(), Document{URI: "wiki://local/concepts/pbl-new-york-guide"}, "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]string)
	for _, item := range result.Suggestions {
		kinds[item.URI] = item.Kind
	}
	if kinds["wiki://local/entities/teacher"] != "related" || kinds["wiki://local/concepts/pbl-new-york-guide-revised"] != "possible_duplicate" || kinds["wiki://local/sources/pbl-new-york-course-details"] != "missing_link" {
		t.Fatalf("suggestions=%+v", result.Suggestions)
	}
	if _, err := client.Suggest(t.Context(), Document{URI: "wiki://other/concepts/pbl-new-york-guide"}, "local", 5); err == nil {
		t.Fatal("cross-space suggest URI accepted")
	}
}
