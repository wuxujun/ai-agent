package wiki

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDirectoryBM25TokensHandleMixedEnglishAndCJK(t *testing.T) {
	got := uniqueDirectoryBM25Tokens("PBL历史旅行指南 2026")
	want := []string{"pbl", "历史", "史旅", "旅行", "行指", "指南", "2026"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens=%v want=%v", got, want)
	}
}

func TestDirectoryBM25RanksExactTitleAndRejectsWeakPartialMatch(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"guide.md": "# PBL 历史旅行指南：纽约篇\n\n面向学生的项目制写作课程。\n",
		"notes.md": "# Wiki Topic Notes\n\nPBL 项目资料与通用 topic 说明。\n",
	}
	for name, content := range pages {
		if err := os.WriteFile(filepath.Join(wikiRoot, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, explanations, err := client.SearchWithExplain(t.Context(), "PBL历史旅行指南", 5, "local")
	if err != nil || len(documents) == 0 || documents[0].Slug != "concepts/guide" {
		t.Fatalf("documents=%+v err=%v", documents, err)
	}
	if len(explanations) != len(documents) || explanations[0].FieldScores["title"] == 0 || explanations[0].FinalScore != documents[0].Score {
		t.Fatalf("explanations=%+v", explanations)
	}
	weak, err := client.Search(t.Context(), "zzqv nonexistent wiki topic alpha 94721", 5, "local")
	if err != nil || len(weak) != 0 {
		t.Fatalf("weak partial match=%+v err=%v", weak, err)
	}
}

func TestDirectoryBM25ChineseQuestionRetrievesFactSentence(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "projects")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "release.md"), []byte("# 发布决策\n\n当前发布负责人是梅琳。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}

	documents, err := client.Search(t.Context(), "请问目前负责上线发布工作的人究竟是谁", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Slug != "projects/release" {
		t.Fatalf("documents=%+v, want projects/release", documents)
	}
}

func TestDirectoryBM25MinimumMatchesRequiresHanDominanceForRecallCap(t *testing.T) {
	tests := []struct {
		name, query  string
		tokens, want int
	}{
		{name: "Han dominant", query: "PBL历史旅行指南 2026", tokens: 7, want: 2},
		{name: "single Han noise", query: "alpha beta gamma delta epsilon 汉", tokens: 6, want: 4},
		{name: "mixed no answer", query: "unknown office address campus room 汉", tokens: 6, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := directoryBM25MinimumMatches(tt.query, tt.tokens); got != tt.want {
				t.Fatalf("minimum matches = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDirectoryBM25SingleHanNoiseDoesNotQualifyWeakDocument(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiRoot, "noise.md"), []byte("# Alpha 汉\n\nUnrelated material.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "alpha beta gamma delta epsilon 汉", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("weak mixed-query result qualified: %+v", documents)
	}
}

func TestDirectoryBM25HanQuestionRequiresDiscriminativeMatch(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "projects")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"release.md":     "# Atlas 项目发布\n\n当前发布负责人是 Mei。\n",
		"preferences.md": "# Atlas 项目偏好\n\n默认使用中文。\n",
		"timeline.md":    "# Atlas 项目时间线\n\n当前截止日已确认。\n",
	} {
		if err := os.WriteFile(filepath.Join(wikiRoot, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "Atlas 项目办公室地址是什么", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("common project terms qualified a no-answer query: %+v", documents)
	}
}

func TestDirectoryBM25EntityQuestionReturnsBothNames(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "entities")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"mei-lin.md":  "# Mei Lin\n\nMei Lin 是发布负责人。\n",
		"mina-lin.md": "# Mina Lin\n\nMina Lin 是安全审阅者。\n",
		"ari.md":      "# Ari Chen\n\nAri Chen 是旧负责人。\n",
		"noor.md":     "# Noor\n\nNoor 是其他负责人。\n",
	} {
		if err := os.WriteFile(filepath.Join(wikiRoot, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "Mei Lin 和 Mina Lin 分别承担什么职责？", 5, "local")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"entities/mei-lin": true, "entities/mina-lin": true}
	for _, document := range documents {
		delete(want, document.Slug)
	}
	if len(want) != 0 {
		t.Fatalf("entity query omitted %v: %+v", want, documents)
	}
}

func TestDirectoryBM25RepositoryEntityQuestionReturnsBothNames(t *testing.T) {
	root := filepath.Join("..", "..", "evals", "brain", "fixtures", "tenant-north", "project-atlas", "brain")
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	query := "Mei Lin 和 Mina Lin 分别承担什么职责？"
	documents, explanations, err := client.SearchWithExplain(t.Context(), query, 8, "atlas-north")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"entities/mei-lin": true, "entities/mina-lin": true}
	for _, document := range documents {
		delete(want, document.Slug)
	}
	if len(want) != 0 {
		snapshot := client.indexSnapshot.Load()
		for _, token := range uniqueDirectoryBM25Tokens(query) {
			t.Logf("token=%q postings=%d", token, len(snapshot.bm25.postings[token]))
		}
		t.Fatalf("entity query omitted %v: documents=%+v explanations=%+v", want, documents, explanations)
	}
}

func TestDirectoryRejectsUnknownSearchMode(t *testing.T) {
	if _, err := NewDirectory(t.TempDir(), WithSearchMode("semantic")); err == nil {
		t.Fatal("unknown search mode accepted")
	}
	if _, err := NewDirectory(t.TempDir(), WithRefreshInterval(-time.Second)); err == nil {
		t.Fatal("negative refresh interval accepted")
	}
	if _, err := NewDirectory(t.TempDir(), WithGraphMaxNodes(maxGraphNodes+1)); err == nil {
		t.Fatal("oversized graph node limit accepted")
	}
}

func TestDirectoryBM25TopKUsesDeterministicSlugTieBreak(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki", "concepts")
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"charlie", "alpha", "bravo", "delta"} {
		content := "# Topic\n\nShared ranking phrase."
		if err := os.WriteFile(filepath.Join(wikiRoot, slug+".md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, explanations, err := client.SearchWithExplain(t.Context(), "Shared ranking phrase", 2, "local")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"concepts/alpha", "concepts/bravo"}
	if len(documents) != len(want) || len(explanations) != len(want) {
		t.Fatalf("documents=%+v explanations=%+v", documents, explanations)
	}
	for index, slug := range want {
		if documents[index].Slug != slug || explanations[index].Slug != slug || explanations[index].FinalScore != documents[index].Score {
			t.Fatalf("index=%d document=%+v explanation=%+v", index, documents[index], explanations[index])
		}
	}
}

func TestDirectoryBM25DemotesNonExactRawSourceVariant(t *testing.T) {
	root := t.TempDir()
	wikiRoot := filepath.Join(root, "wiki")
	for _, category := range []string{"concepts", "sources"} {
		if err := os.MkdirAll(filepath.Join(wikiRoot, category), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pages := map[string]string{
		"concepts/pbl-new-york.md":          "# PBL 历史旅行指南：纽约篇\n\n目标课程。",
		"sources/pbl-new-york-course.md":    "# PBL 历史旅行指南：纽约篇课程来源\n\nVanessa Ruales。",
		"concepts/course-design.md":         "# 项目制课程设计\n\nPBL 历史旅行指南 纽约课程设计。",
		"sources/raw-doc-similar-course.md": "# 新课堂历史旅行指南写作课程介绍来源说明\n\nPBL 历史旅行指南，1920年代纽约，导师 Estelle。",
	}
	for name, content := range pages {
		if err := os.WriteFile(filepath.Join(wikiRoot, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewDirectory(root, WithSearchMode(SearchModeBM25))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, explanations, err := client.SearchWithExplain(t.Context(), "PBL 历史旅行指南 纽约", 3, "local")
	if err != nil || len(documents) != 3 {
		t.Fatalf("documents=%+v err=%v", documents, err)
	}
	for _, document := range documents {
		if document.Slug == "sources/raw-doc-similar-course" {
			t.Fatalf("non-exact raw source entered top 3: %+v", documents)
		}
	}
	exactDocuments, exactExplanations, err := client.SearchWithExplain(t.Context(), "新课堂历史旅行指南写作课程介绍来源说明", 1, "local")
	if err != nil || len(exactDocuments) != 1 || exactDocuments[0].Slug != "sources/raw-doc-similar-course" || exactExplanations[0].SourceMultiplier != 1 {
		t.Fatalf("exact raw source=%+v explanations=%+v err=%v", exactDocuments, exactExplanations, err)
	}
	foundPenalty := false
	for _, explanation := range explanations {
		foundPenalty = foundPenalty || explanation.SourceMultiplier < 1
	}
	if foundPenalty {
		t.Fatal("penalized raw source should not have entered explained top 3")
	}
}
