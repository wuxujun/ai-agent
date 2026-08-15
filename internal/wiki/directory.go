package wiki

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/policy"
)

// DirectoryClient reads an llm-wiki checkout without requiring its MCP
// server. It exposes only Search and Read and never modifies the checkout.
type DirectoryClient struct {
	configuredRoot string
	root           string
}

func NewDirectory(root string) (*DirectoryClient, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("wiki directory must not be empty")
	}
	return &DirectoryClient{configuredRoot: root}, nil
}

func (c *DirectoryClient) Initialize(context.Context) error {
	root, err := filepath.Abs(filepath.Clean(c.configuredRoot))
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(root, "wiki")); statErr == nil && info.IsDir() {
		root = filepath.Join(root, "wiki")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("open wiki directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("wiki directory %q is not a directory", root)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve wiki directory: %w", err)
	}
	c.root = resolved
	return nil
}

func (c *DirectoryClient) Close(context.Context) error { return nil }

func (c *DirectoryClient) Search(ctx context.Context, query string, topK int, space string) ([]Document, error) {
	if c.root == "" {
		return nil, errors.New("wiki directory client is not initialized")
	}
	terms := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(query)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(terms) == 0 {
		return nil, errors.New("wiki search query must not be empty")
	}
	if topK <= 0 {
		topK = 5
	}
	space = strings.Trim(strings.TrimSpace(space), "/")
	if space == "" {
		space = "local"
	}
	var documents []Document
	err := filepath.WalkDir(c.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			return nil
		}
		if err := policy.ValidateReadPath(c.root, path); err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(content) > 2<<20 {
			content = content[:2<<20]
		}
		relative, err := filepath.Rel(c.root, path)
		if err != nil {
			return err
		}
		slug := strings.TrimSuffix(filepath.ToSlash(relative), filepath.Ext(relative))
		haystack := strings.ToLower(slug + "\n" + string(content))
		score := 0
		for _, term := range terms {
			score += strings.Count(haystack, term)
		}
		if score == 0 {
			return nil
		}
		title, excerpt := directoryPageSummary(slug, string(content))
		documents = append(documents, Document{Slug: slug, URI: "wiki://" + space + "/" + slug, Title: title, Excerpt: excerpt, Score: float64(score)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(documents, func(i, j int) bool {
		if documents[i].Score == documents[j].Score {
			return documents[i].Slug < documents[j].Slug
		}
		return documents[i].Score > documents[j].Score
	})
	if len(documents) > topK {
		documents = documents[:topK]
	}
	return documents, nil
}

func (c *DirectoryClient) Read(ctx context.Context, document Document, space string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	slug := strings.TrimSpace(document.Slug)
	if slug == "" {
		slug = slugFromWikiURI(document.URI, space)
	}
	if slug == "" || filepath.IsAbs(slug) {
		return Document{}, errors.New("wiki document has no valid relative slug")
	}
	path := filepath.Join(c.root, filepath.FromSlash(slug))
	if filepath.Ext(path) == "" {
		path += ".md"
	}
	if err := policy.ValidateReadPath(c.root, path); err != nil {
		return Document{}, fmt.Errorf("wiki page path rejected: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read wiki page %q: %w", slug, err)
	}
	document.Slug = strings.TrimSuffix(filepath.ToSlash(slug), filepath.Ext(slug))
	if document.URI == "" {
		space = firstNonEmpty(strings.Trim(space, "/"), "local")
		document.URI = "wiki://" + space + "/" + document.Slug
	}
	document.Content = string(content)
	return document, nil
}

func directoryPageSummary(slug, content string) (string, string) {
	title := filepath.Base(slug)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			break
		}
	}
	excerpt := strings.TrimSpace(content)
	if len(excerpt) > 500 {
		excerpt = excerpt[:500]
	}
	return title, excerpt
}
