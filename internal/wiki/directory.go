package wiki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/policy"
	"gopkg.in/yaml.v3"
)

// DirectoryClient reads an llm-wiki checkout without requiring its MCP
// server. It exposes only Search and Read and never modifies the checkout.
type DirectoryClient struct {
	configuredRoot string
	root           string
	indexMu        sync.RWMutex
	indexSignature string
	index          []directoryIndexPage
}

type directoryIndexPage struct {
	slug            string
	title           string
	summary         string
	excerpt         string
	links           []string
	slugText        string
	titleText       string
	headingText     string
	metadataText    string
	bodyText        string
	slugCompact     string
	titleCompact    string
	metadataCompact string
	bodyCompact     string
	chunks          []directoryIndexChunk
}

type directoryIndexChunk struct {
	text       string
	normalized string
	compact    string
}

type directoryFrontmatter struct {
	Title   string   `yaml:"title"`
	Summary string   `yaml:"summary"`
	Tags    []string `yaml:"tags"`
	Aliases []string `yaml:"aliases"`
}

var markdownWikiLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)#]+\.md)(?:#[^)]*)?\)`)

const maxDirectoryPageBytes = 2 << 20

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
	_, err = c.ensureIndex(context.Background())
	return err
}

func (c *DirectoryClient) Close(context.Context) error { return nil }

func (c *DirectoryClient) SupportsGraph() bool   { return c != nil }
func (c *DirectoryClient) SupportsSuggest() bool { return c != nil }

// Probe verifies that the local Wiki root remains readable and refreshes the
// index when its Markdown inventory changed.
func (c *DirectoryClient) Probe(ctx context.Context) error {
	if c == nil || c.root == "" {
		return errors.New("wiki directory is not initialized")
	}
	_, err := c.ensureIndex(ctx)
	return err
}

func (c *DirectoryClient) Search(ctx context.Context, query string, topK int, space string) ([]Document, error) {
	if c.root == "" {
		return nil, errors.New("wiki directory client is not initialized")
	}
	terms := directorySearchTerms(query)
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
	pages, err := c.ensureIndex(ctx)
	if err != nil {
		return nil, err
	}
	phrase := strings.Join(terms, " ")
	compactPhrase := compactDirectorySearchText(phrase)
	scores := make(map[string]float64, len(pages))
	pageBySlug := make(map[string]directoryIndexPage, len(pages))
	for _, page := range pages {
		pageBySlug[page.slug] = page
		scores[page.slug] = scoreDirectoryPage(page, terms, phrase, compactPhrase)
	}
	// A strongly matching compiled page can nominate its directly linked source,
	// entity, concept, or comparison pages. This is deliberately one hop and
	// capped so relationship context cannot outrank a direct title match.
	for _, page := range pages {
		directScore := scores[page.slug]
		if directScore == 0 || isDirectoryNavigationPage(page.slug) {
			continue
		}
		linkBoost := directScore * 0.08
		if linkBoost > 25 {
			linkBoost = 25
		}
		for _, linkedSlug := range page.links {
			if _, exists := pageBySlug[linkedSlug]; exists {
				scores[linkedSlug] += linkBoost
			}
		}
	}
	documents := make([]Document, 0, len(pages))
	for _, page := range pages {
		score := scores[page.slug]
		if score == 0 {
			continue
		}
		if isDirectoryNavigationPage(page.slug) {
			score *= 0.25
		}
		excerpt := bestDirectoryExcerpt(page, terms, phrase, compactPhrase)
		documents = append(documents, Document{Slug: page.slug, URI: "wiki://" + space + "/" + page.slug, Title: page.title, Summary: page.summary, Excerpt: excerpt, Score: score})
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

// Graph returns a bounded, read-only link neighborhood from the directory
// index. Incoming edges are derived from backlinks in the same snapshot.
func (c *DirectoryClient) Graph(ctx context.Context, document Document, space string, depth int, direction string) (GraphResult, error) {
	if c == nil || c.root == "" {
		return GraphResult{}, errors.New("wiki directory client is not initialized")
	}
	if strings.TrimSpace(space) == "" {
		space = "local"
	}
	space = strings.Trim(strings.TrimSpace(space), "/")
	root, direction, err := validateGraphRequest(document, space, depth, direction)
	if err != nil {
		return GraphResult{}, err
	}
	pages, err := c.ensureIndex(ctx)
	if err != nil {
		return GraphResult{}, err
	}
	pageBySlug := make(map[string]directoryIndexPage, len(pages))
	incoming := make(map[string][]string, len(pages))
	for _, page := range pages {
		pageBySlug[page.slug] = page
		for _, target := range page.links {
			incoming[target] = append(incoming[target], page.slug)
		}
	}
	if _, ok := pageBySlug[root]; !ok {
		return GraphResult{}, fmt.Errorf("wiki graph root %q was not found", root)
	}
	seen := map[string]bool{root: true}
	frontier := []string{root}
	edges := make([]GraphEdge, 0)
	for level := 0; level < depth && len(frontier) > 0; level++ {
		next := make([]string, 0)
		for _, slug := range frontier {
			if direction == "outgoing" || direction == "both" {
				for _, target := range pageBySlug[slug].links {
					if _, ok := pageBySlug[target]; !ok {
						continue
					}
					edges = append(edges, GraphEdge{From: graphURI(space, slug), To: graphURI(space, target)})
					if !seen[target] {
						seen[target] = true
						next = append(next, target)
					}
				}
			}
			if direction == "incoming" || direction == "both" {
				for _, source := range incoming[slug] {
					edges = append(edges, GraphEdge{From: graphURI(space, source), To: graphURI(space, slug)})
					if !seen[source] {
						seen[source] = true
						next = append(next, source)
					}
				}
			}
		}
		frontier = next
	}
	nodes := make([]GraphNode, 0, len(seen))
	for slug := range seen {
		page := pageBySlug[slug]
		nodes = append(nodes, GraphNode{URI: graphURI(space, slug), Slug: slug, Title: page.title, Summary: page.summary})
	}
	return normalizeGraphResult(GraphResult{RootURI: graphURI(space, root), Nodes: nodes, Edges: edges}), nil
}

// Suggest returns bounded, read-only curation candidates. It never modifies
// links or pages; callers decide whether a suggestion is useful.
func (c *DirectoryClient) Suggest(ctx context.Context, document Document, space string, limit int) (SuggestResult, error) {
	if c == nil || c.root == "" {
		return SuggestResult{}, errors.New("wiki directory client is not initialized")
	}
	slug, space, limit, err := validateSuggestRequest(document, space, limit)
	if err != nil {
		return SuggestResult{}, err
	}
	pages, err := c.ensureIndex(ctx)
	if err != nil {
		return SuggestResult{}, err
	}
	pageBySlug := make(map[string]directoryIndexPage, len(pages))
	direct := make(map[string]bool)
	for _, page := range pages {
		pageBySlug[page.slug] = page
		if page.slug == slug {
			for _, target := range page.links {
				direct[target] = true
			}
		}
		for _, target := range page.links {
			if target == slug {
				direct[page.slug] = true
			}
		}
	}
	root, ok := pageBySlug[slug]
	if !ok {
		return SuggestResult{}, fmt.Errorf("wiki suggest root %q was not found", slug)
	}
	result := SuggestResult{RootURI: graphURI(space, slug)}
	for linkedSlug := range direct {
		if page, exists := pageBySlug[linkedSlug]; exists {
			result.Suggestions = append(result.Suggestions, Suggestion{Kind: "related", URI: graphURI(space, linkedSlug), Title: page.title, Reason: "direct Wiki link relationship", Score: 1})
		}
	}
	candidates, err := c.Search(ctx, root.title, min(20, len(pages)), space)
	if err != nil {
		return SuggestResult{}, err
	}
	maxScore := float64(1)
	if len(candidates) > 0 && candidates[0].Score > maxScore {
		maxScore = candidates[0].Score
	}
	for _, candidate := range candidates {
		if candidate.Slug == slug || direct[candidate.Slug] {
			continue
		}
		similarity := titleSimilarity(root.title, candidate.Title)
		item := Suggestion{URI: candidate.URI, Title: candidate.Title, Score: candidate.Score / maxScore}
		if similarity >= 0.6 {
			item.Kind, item.Reason, item.Score = "possible_duplicate", "title substantially overlaps the root page", similarity
		} else {
			item.Kind, item.Reason = "missing_link", "search-relevant page has no direct link to the root"
		}
		result.Suggestions = append(result.Suggestions, item)
	}
	return normalizeSuggestResult(result, limit), nil
}

func (c *DirectoryClient) ensureIndex(ctx context.Context) ([]directoryIndexPage, error) {
	paths, signature, err := directoryMarkdownFiles(ctx, c.root)
	if err != nil {
		return nil, err
	}
	c.indexMu.RLock()
	if signature == c.indexSignature {
		pages := append([]directoryIndexPage(nil), c.index...)
		c.indexMu.RUnlock()
		return pages, nil
	}
	c.indexMu.RUnlock()

	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	// Another concurrent search may already have rebuilt this signature.
	if signature == c.indexSignature {
		return append([]directoryIndexPage(nil), c.index...), nil
	}
	pages := make([]directoryIndexPage, 0, len(paths))
	for _, path := range paths {
		page, err := buildDirectoryIndexPage(c.root, path)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	c.index = pages
	c.indexSignature = signature
	return append([]directoryIndexPage(nil), c.index...), nil
}

func directoryMarkdownFiles(ctx context.Context, root string) ([]string, string, error) {
	var paths []string
	var signature strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			return nil
		}
		if err := policy.ValidateReadPath(root, path); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		paths = append(paths, path)
		fmt.Fprintf(&signature, "%s\x00%d\x00%d\n", path, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return paths, signature.String(), err
}

func buildDirectoryIndexPage(root, path string) (directoryIndexPage, error) {
	content, err := readDirectoryMarkdownFile(path)
	if err != nil {
		return directoryIndexPage{}, fmt.Errorf("index wiki page %q: %w", path, err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return directoryIndexPage{}, err
	}
	slug := strings.TrimSuffix(filepath.ToSlash(relative), filepath.Ext(relative))
	metadata, body := parseDirectoryFrontmatter(string(content))
	title, excerpt := directoryPageSummary(slug, body)
	if strings.TrimSpace(metadata.Title) != "" {
		title = strings.TrimSpace(metadata.Title)
	}
	summary := strings.TrimSpace(metadata.Summary)
	if summary == "" {
		summary = excerpt
	}
	var headings []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			headings = append(headings, strings.TrimSpace(strings.TrimLeft(line, "#")))
		}
	}
	page := directoryIndexPage{
		slug: slug, title: title, summary: summary, excerpt: excerpt, links: directoryPageLinks(slug, body),
		slugText: normalizeDirectorySearchText(slug), titleText: normalizeDirectorySearchText(title),
		headingText:  normalizeDirectorySearchText(strings.Join(headings, " ")),
		metadataText: normalizeDirectorySearchText(strings.Join(append(append([]string{metadata.Summary}, metadata.Tags...), metadata.Aliases...), " ")),
		bodyText:     normalizeDirectorySearchText(body),
	}
	page.slugCompact = strings.ReplaceAll(page.slugText, " ", "")
	page.titleCompact = strings.ReplaceAll(page.titleText, " ", "")
	page.metadataCompact = strings.ReplaceAll(page.metadataText, " ", "")
	page.bodyCompact = strings.ReplaceAll(page.bodyText, " ", "")
	page.chunks = directorySearchChunks(body)
	return page, nil
}

func directorySearchChunks(body string) []directoryIndexChunk {
	const maxChunks = 256
	paragraphs := strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' })
	chunks := make([]directoryIndexChunk, 0, min(len(paragraphs), maxChunks))
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		runes := []rune(paragraph)
		if len(runes) > 500 {
			runes = runes[:500]
			paragraph = string(runes)
		}
		normalized := normalizeDirectorySearchText(paragraph)
		chunks = append(chunks, directoryIndexChunk{text: paragraph, normalized: normalized, compact: strings.ReplaceAll(normalized, " ", "")})
		if len(chunks) == maxChunks {
			break
		}
	}
	return chunks
}

func bestDirectoryExcerpt(page directoryIndexPage, terms []string, phrase, compactPhrase string) string {
	bestScore := 0
	best := ""
	for _, chunk := range page.chunks {
		score := 0
		if phrase != "" && strings.Contains(chunk.normalized, phrase) {
			score += 20
		}
		if compactPhrase != "" && strings.Contains(chunk.compact, compactPhrase) {
			score += 10
		}
		for _, term := range terms {
			if strings.Contains(chunk.normalized, term) {
				score += 3
			}
		}
		if score > bestScore {
			bestScore = score
			best = chunk.text
		}
	}
	if best != "" {
		return best
	}
	return page.excerpt
}

func parseDirectoryFrontmatter(content string) (directoryFrontmatter, string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return directoryFrontmatter{}, content
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return directoryFrontmatter{}, content
	}
	end += 4
	var metadata directoryFrontmatter
	if err := yaml.Unmarshal([]byte(normalized[4:end]), &metadata); err != nil {
		return directoryFrontmatter{}, content
	}
	return metadata, normalized[end+5:]
}

func directoryPageLinks(sourceSlug, content string) []string {
	seen := make(map[string]bool)
	var links []string
	for _, match := range markdownWikiLinkPattern.FindAllStringSubmatch(content, -1) {
		if len(match) < 2 {
			continue
		}
		target := strings.TrimSpace(match[1])
		if strings.Contains(target, "://") || filepath.IsAbs(target) {
			continue
		}
		resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(sourceSlug), filepath.FromSlash(target))))
		resolved = strings.TrimSuffix(resolved, filepath.Ext(resolved))
		if resolved == ".." || strings.HasPrefix(resolved, "../") || seen[resolved] {
			continue
		}
		seen[resolved] = true
		links = append(links, resolved)
	}
	return links
}

func isDirectoryNavigationPage(slug string) bool {
	base := strings.ToLower(filepath.Base(slug))
	return base == "index" || base == "_index" || base == "log"
}

func directorySearchTerms(value string) []string {
	return strings.Fields(normalizeDirectorySearchText(value))
}

func normalizeDirectorySearchText(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

func compactDirectorySearchText(value string) string {
	return strings.ReplaceAll(normalizeDirectorySearchText(value), " ", "")
}

func scoreDirectoryPage(page directoryIndexPage, terms []string, phrase, compactPhrase string) float64 {
	score := 0
	if phrase != "" {
		if strings.Contains(page.titleText, phrase) {
			score += 100
		}
		if strings.Contains(page.slugText, phrase) {
			score += 70
		}
		if strings.Contains(page.headingText, phrase) {
			score += 40
		}
		if strings.Contains(page.metadataText, phrase) {
			score += 35
		}
		if strings.Contains(page.bodyText, phrase) {
			score += 20
		}
	}
	// Compact matching bridges harmless punctuation and spacing differences,
	// especially mixed acronym/CJK queries such as "PBL历史旅行指南". Its
	// weights remain below normalized exact-phrase weights.
	if compactPhrase != "" {
		if strings.Contains(page.titleCompact, compactPhrase) {
			score += 60
		}
		if strings.Contains(page.slugCompact, compactPhrase) {
			score += 40
		}
		if strings.Contains(page.metadataCompact, compactPhrase) {
			score += 20
		}
		if strings.Contains(page.bodyCompact, compactPhrase) {
			score += 10
		}
	}
	matched := 0
	for _, term := range terms {
		termScore := strings.Count(page.titleText, term)*25 + strings.Count(page.slugText, term)*18 + strings.Count(page.headingText, term)*10 + strings.Count(page.metadataText, term)*8
		bodyMatches := strings.Count(page.bodyText, term)
		if bodyMatches > 10 {
			bodyMatches = 10
		}
		termScore += bodyMatches * 2
		if termScore > 0 {
			matched++
			score += termScore
		}
	}
	if matched == len(terms) {
		score += 25
	}
	return float64(score)
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
	content, err := readDirectoryMarkdownFile(path)
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

func readDirectoryMarkdownFile(path string) ([]byte, error) {
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		return nil, errors.New("wiki page must be a Markdown file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("wiki page is not a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxDirectoryPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxDirectoryPageBytes {
		return nil, fmt.Errorf("wiki page exceeds %d byte read limit", maxDirectoryPageBytes)
	}
	return content, nil
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
	excerptRunes := []rune(strings.TrimSpace(content))
	if len(excerptRunes) > 500 {
		excerptRunes = excerptRunes[:500]
	}
	return title, string(excerptRunes)
}
