package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkDirectoryClientSearch(b *testing.B) {
	queries := []struct {
		name  string
		query string
	}{
		{name: "english", query: "Course Topic 42 historical travel"},
		{name: "han", query: "PBL 历史旅行指南 模块 42"},
		{name: "mixed-noise", query: "Course Topic 42 historical travel 汉"},
	}
	for _, mode := range []string{SearchModeLegacy, SearchModeBM25} {
		for _, pages := range []int{100, 1_000, 10_000} {
			for _, query := range queries {
				b.Run(fmt.Sprintf("%s/pages-%d/%s", mode, pages, query.name), func(b *testing.B) {
					root := b.TempDir()
					wikiRoot := filepath.Join(root, "wiki", "concepts")
					if err := os.MkdirAll(wikiRoot, 0o755); err != nil {
						b.Fatal(err)
					}
					for index := 0; index < pages; index++ {
						content := fmt.Sprintf("# Course Topic %d / PBL 历史旅行指南 模块 %d\n\nHistorical travel guide module %d with curriculum notes.\n", index, index, index)
						path := filepath.Join(wikiRoot, fmt.Sprintf("course-%05d.md", index))
						if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
							b.Fatal(err)
						}
					}
					client, err := NewDirectory(root, WithSearchMode(mode))
					if err != nil {
						b.Fatal(err)
					}
					if err := client.Initialize(b.Context()); err != nil {
						b.Fatal(err)
					}
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						results, err := client.Search(b.Context(), query.query, 5, "local")
						if err != nil || len(results) == 0 {
							b.Fatalf("results=%d err=%v", len(results), err)
						}
					}
				})
			}
		}
	}
}
