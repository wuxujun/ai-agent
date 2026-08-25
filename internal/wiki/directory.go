package wiki

import (
	"context"
	"crypto/sha256"
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
	"sync/atomic"
	"time"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/policy"
	"gopkg.in/yaml.v3"
)

// DirectoryClient reads an llm-wiki checkout without requiring its MCP
// server. It exposes only Search and Read and never modifies the checkout.
type DirectoryClient struct {
	configuredRoot  string
	root            string
	searchMode      string
	refreshInterval time.Duration
	graphMaxNodes   int
	refreshMu       sync.Mutex
	nextRefresh     atomic.Int64
	indexSnapshot   atomic.Pointer[directoryIndexSnapshot]
	refreshAttempt  atomic.Int64
	refreshSuccess  atomic.Int64
	refreshDuration atomic.Int64
	refreshFailures atomic.Int64
}

const (
	SearchModeLegacy = "legacy"
	SearchModeBM25   = "bm25"
)

type DirectoryOption func(*DirectoryClient) error

const defaultDirectoryRefreshInterval = 30 * time.Second

type directoryIndexSnapshot struct {
	signature string
	version   string
	builtAtMS int64
	pages     []directoryIndexPage
	bm25      *directoryBM25Index
}

// DirectoryStatus is a content-free operational view of the local index.
// It intentionally excludes filesystem paths, titles, excerpts, and queries.
type DirectoryStatus struct {
	Backend                    string `json:"backend"`
	SearchMode                 string `json:"search_mode"`
	PageCount                  int    `json:"page_count"`
	IndexVersion               string `json:"index_version,omitempty"`
	IndexBuiltAtUnixMS         int64  `json:"index_built_at_unix_ms,omitempty"`
	LastRefreshAttemptUnixMS   int64  `json:"last_refresh_attempt_unix_ms,omitempty"`
	LastRefreshSuccessUnixMS   int64  `json:"last_refresh_success_unix_ms,omitempty"`
	LastRefreshDurationMS      int64  `json:"last_refresh_duration_ms"`
	ConsecutiveRefreshFailures int64  `json:"consecutive_refresh_failures"`
	ServingStaleSnapshot       bool   `json:"serving_stale_snapshot"`
	RefreshIntervalSeconds     int64  `json:"refresh_interval_seconds"`
	GraphMaxNodes              int    `json:"graph_max_nodes"`
}

// WithSearchMode selects the local ranking implementation. Legacy remains the
// default until the BM25 rollout passes the repository quality gates.
func WithSearchMode(mode string) DirectoryOption {
	return func(client *DirectoryClient) error {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode == "" {
			mode = SearchModeLegacy
		}
		if mode != SearchModeLegacy && mode != SearchModeBM25 {
			return fmt.Errorf("wiki local search mode must be legacy or bm25")
		}
		client.searchMode = mode
		return nil
	}
}

// WithRefreshInterval controls how often normal reads check the directory for
// changes. Zero disables automatic checks; Probe always performs one.
func WithRefreshInterval(interval time.Duration) DirectoryOption {
	return func(client *DirectoryClient) error {
		if interval < 0 {
			return fmt.Errorf("wiki local refresh interval must not be negative")
		}
		client.refreshInterval = interval
		return nil
	}
}

// WithGraphMaxNodes bounds local graph expansion. Zero keeps the default.
func WithGraphMaxNodes(limit int) DirectoryOption {
	return func(client *DirectoryClient) error {
		if limit < 0 || limit > maxGraphNodes {
			return fmt.Errorf("wiki local graph max nodes must be between 0 and %d", maxGraphNodes)
		}
		if limit > 0 {
			client.graphMaxNodes = limit
		}
		return nil
	}
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

// SearchExplanation describes how the deterministic local scorer ranked one
// returned page. It is intended for evaluation and diagnostics; normal Wiki
// tools continue to expose only Document results.
type SearchExplanation struct {
	URI                  string             `json:"uri"`
	Slug                 string             `json:"slug"`
	IndexVersion         string             `json:"index_version"`
	BaseScore            float64            `json:"base_score"`
	LinkBoost            float64            `json:"link_boost,omitempty"`
	NavigationMultiplier float64            `json:"navigation_multiplier"`
	SourceMultiplier     float64            `json:"source_multiplier"`
	FinalScore           float64            `json:"final_score"`
	FieldScores          map[string]float64 `json:"field_scores,omitempty"`
	PhraseMatches        []string           `json:"phrase_matches,omitempty"`
	MatchedTerms         []string           `json:"matched_terms,omitempty"`
}

type directoryScoreExplanation struct {
	fieldScores   map[string]float64
	phraseMatches []string
	matchedTerms  []string
	baseScore     float64
	linkBoost     float64
}

type directoryFrontmatter struct {
	Title   string   `yaml:"title"`
	Summary string   `yaml:"summary"`
	Tags    []string `yaml:"tags"`
	Aliases []string `yaml:"aliases"`
}

var markdownWikiLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)#]+\.md)(?:#[^)]*)?\)`)

const maxDirectoryPageBytes = 2 << 20
const defaultDirectoryGraphMaxNodes = 12

func NewDirectory(root string, options ...DirectoryOption) (*DirectoryClient, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("wiki directory must not be empty")
	}
	client := &DirectoryClient{
		configuredRoot: root, searchMode: SearchModeLegacy,
		refreshInterval: defaultDirectoryRefreshInterval, graphMaxNodes: defaultDirectoryGraphMaxNodes,
	}
	for _, option := range options {
		if option != nil {
			if err := option(client); err != nil {
				return nil, err
			}
		}
	}
	return client, nil
}

func (c *DirectoryClient) Initialize(ctx context.Context) error {
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
	_, err = c.refreshIndex(ctx)
	return err
}

func (c *DirectoryClient) Close(context.Context) error { return nil }

func (c *DirectoryClient) Status() DirectoryStatus {
	status := DirectoryStatus{
		Backend: "directory", SearchMode: c.searchMode,
		LastRefreshAttemptUnixMS: c.refreshAttempt.Load(), LastRefreshSuccessUnixMS: c.refreshSuccess.Load(),
		LastRefreshDurationMS: c.refreshDuration.Load(), ConsecutiveRefreshFailures: c.refreshFailures.Load(),
		RefreshIntervalSeconds: int64(c.refreshInterval / time.Second), GraphMaxNodes: c.graphMaxNodes,
	}
	if snapshot := c.indexSnapshot.Load(); snapshot != nil {
		status.PageCount = len(snapshot.pages)
		status.IndexVersion = snapshot.version
		status.IndexBuiltAtUnixMS = snapshot.builtAtMS
		status.ServingStaleSnapshot = status.ConsecutiveRefreshFailures > 0
	}
	return status
}

func (c *DirectoryClient) SupportsGraph() bool   { return c != nil }
func (c *DirectoryClient) SupportsSuggest() bool { return c != nil }

// Probe verifies that the local Wiki root remains readable and refreshes the
// index when its Markdown inventory changed.
func (c *DirectoryClient) Probe(ctx context.Context) error {
	if c == nil || c.root == "" {
		return errors.New("wiki directory is not initialized")
	}
	_, err := c.refreshIndex(ctx)
	return err
}

func (c *DirectoryClient) Search(ctx context.Context, query string, topK int, space string) ([]Document, error) {
	documents, _, err := c.search(ctx, query, topK, space, false)
	return documents, err
}

// SearchWithExplain returns the same ranked documents as Search plus bounded,
// path-free scoring details for offline evaluation and diagnostics.
func (c *DirectoryClient) SearchWithExplain(ctx context.Context, query string, topK int, space string) ([]Document, []SearchExplanation, error) {
	return c.search(ctx, query, topK, space, true)
}

func (c *DirectoryClient) search(ctx context.Context, query string, topK int, space string, explain bool) ([]Document, []SearchExplanation, error) {
	if c.root == "" {
		return nil, nil, errors.New("wiki directory client is not initialized")
	}
	terms := directorySearchTerms(query)
	if len(terms) == 0 {
		return nil, nil, errors.New("wiki search query must not be empty")
	}
	if topK <= 0 {
		topK = 5
	}
	space = strings.Trim(strings.TrimSpace(space), "/")
	if space == "" {
		space = "local"
	}
	snapshot, err := c.currentIndexSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	pages, bm25Index := snapshot.pages, snapshot.bm25
	indexVersion := ""
	if explain {
		indexVersion = snapshot.version
	}
	phrase := strings.Join(terms, " ")
	compactPhrase := compactDirectorySearchText(phrase)
	if c.searchMode == SearchModeBM25 {
		return c.searchBM25(query, terms, phrase, compactPhrase, topK, space, pages, bm25Index, indexVersion, explain)
	}
	scores := make(map[string]float64, len(pages))
	details := make(map[string]*directoryScoreExplanation, len(pages))
	pageBySlug := make(map[string]directoryIndexPage, len(pages))
	for _, page := range pages {
		pageBySlug[page.slug] = page
		if explain {
			detail := scoreDirectoryPageDetailed(page, terms, phrase, compactPhrase)
			scores[page.slug], details[page.slug] = detail.baseScore, &detail
		} else {
			scores[page.slug] = scoreDirectoryPage(page, terms, phrase, compactPhrase)
		}
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
				if explain {
					details[linkedSlug].linkBoost += linkBoost
				}
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
	if !explain {
		return documents, nil, nil
	}
	explanations := make([]SearchExplanation, 0, len(documents))
	for _, document := range documents {
		detail := details[document.Slug]
		multiplier := float64(1)
		if isDirectoryNavigationPage(document.Slug) {
			multiplier = 0.25
		}
		explanations = append(explanations, SearchExplanation{
			URI: document.URI, Slug: document.Slug, IndexVersion: indexVersion,
			BaseScore: detail.baseScore, LinkBoost: detail.linkBoost,
			NavigationMultiplier: multiplier, SourceMultiplier: 1, FinalScore: document.Score,
			FieldScores: detail.fieldScores, PhraseMatches: detail.phraseMatches,
			MatchedTerms: detail.matchedTerms,
		})
	}
	return documents, explanations, nil
}

func directoryIndexVersion(pages []directoryIndexPage) string {
	hash := sha256.New()
	for _, page := range pages {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\n", page.slug, page.titleText, page.metadataText, page.bodyText)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)[:12])
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
	snapshot, err := c.currentIndexSnapshot(ctx)
	if err != nil {
		return GraphResult{}, err
	}
	pages := snapshot.pages
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
	rootPage := pageBySlug[root]
	rootTerms := directorySearchTerms(rootPage.title)
	rootPhrase := strings.Join(rootTerms, " ")
	rootCompactPhrase := compactDirectorySearchText(rootPhrase)
	for level := 0; level < depth && len(frontier) > 0; level++ {
		pendingEdges := make(map[string][]GraphEdge)
		for _, slug := range frontier {
			if direction == "outgoing" || direction == "both" {
				for _, target := range pageBySlug[slug].links {
					if _, ok := pageBySlug[target]; !ok {
						continue
					}
					edge := GraphEdge{From: graphURI(space, slug), To: graphURI(space, target)}
					if seen[target] {
						edges = append(edges, edge)
					} else {
						pendingEdges[target] = append(pendingEdges[target], edge)
					}
				}
			}
			if direction == "incoming" || direction == "both" {
				for _, source := range incoming[slug] {
					edge := GraphEdge{From: graphURI(space, source), To: graphURI(space, slug)}
					if seen[source] {
						edges = append(edges, edge)
					} else {
						pendingEdges[source] = append(pendingEdges[source], edge)
					}
				}
			}
		}
		candidates := make([]string, 0, len(pendingEdges))
		for slug := range pendingEdges {
			candidates = append(candidates, slug)
		}
		sort.Slice(candidates, func(left, right int) bool {
			leftScore := directoryGraphRelevance(pageBySlug[candidates[left]], rootTerms, rootPhrase, rootCompactPhrase)
			rightScore := directoryGraphRelevance(pageBySlug[candidates[right]], rootTerms, rootPhrase, rootCompactPhrase)
			if leftScore == rightScore {
				return candidates[left] < candidates[right]
			}
			return leftScore > rightScore
		})
		remaining := c.graphMaxNodes - len(seen)
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
		}
		for _, slug := range candidates {
			seen[slug] = true
			edges = append(edges, pendingEdges[slug]...)
		}
		frontier = candidates
	}
	nodes := make([]GraphNode, 0, len(seen))
	for slug := range seen {
		page := pageBySlug[slug]
		nodes = append(nodes, GraphNode{URI: graphURI(space, slug), Slug: slug, Title: page.title, Summary: page.summary})
	}
	return normalizeGraphResult(GraphResult{RootURI: graphURI(space, root), Nodes: nodes, Edges: edges}), nil
}

func directoryGraphRelevance(page directoryIndexPage, terms []string, phrase, compactPhrase string) float64 {
	score := scoreDirectoryPage(page, terms, phrase, compactPhrase)
	if isDirectoryNavigationPage(page.slug) {
		score *= 0.25
	}
	return score
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
	snapshot, err := c.currentIndexSnapshot(ctx)
	if err != nil {
		return SuggestResult{}, err
	}
	pages := snapshot.pages
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

func (c *DirectoryClient) currentIndexSnapshot(ctx context.Context) (*directoryIndexSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot := c.indexSnapshot.Load()
	if snapshot == nil {
		return c.refreshIndex(ctx)
	}
	if c.refreshInterval <= 0 {
		return snapshot, nil
	}
	now := time.Now()
	next := c.nextRefresh.Load()
	if now.UnixNano() < next || !c.nextRefresh.CompareAndSwap(next, now.Add(c.refreshInterval).UnixNano()) {
		return snapshot, nil
	}
	refreshed, err := c.refreshIndex(ctx)
	if err == nil {
		return refreshed, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Ordinary reads fail open to the last complete immutable snapshot. Probe
	// remains the explicit health/refresh path and reports the indexing error.
	return snapshot, nil
}

func (c *DirectoryClient) refreshIndex(ctx context.Context) (result *directoryIndexSnapshot, err error) {
	started := time.Now()
	c.refreshAttempt.Store(started.UnixMilli())
	defer func() {
		c.refreshDuration.Store(time.Since(started).Milliseconds())
		if err != nil {
			c.refreshFailures.Add(1)
			return
		}
		c.refreshFailures.Store(0)
		c.refreshSuccess.Store(time.Now().UnixMilli())
	}()
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	paths, signature, err := directoryMarkdownFiles(ctx, c.root)
	if err != nil {
		return nil, err
	}
	current := c.indexSnapshot.Load()
	if current != nil && signature == current.signature {
		c.scheduleNextRefresh()
		return current, nil
	}
	pages := make([]directoryIndexPage, 0, len(paths))
	for _, path := range paths {
		page, err := buildDirectoryIndexPage(c.root, path)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	snapshot := &directoryIndexSnapshot{signature: signature, version: directoryIndexVersion(pages), builtAtMS: time.Now().UnixMilli(), pages: pages}
	if c.searchMode == SearchModeBM25 {
		snapshot.bm25 = buildDirectoryBM25Index(pages)
	}
	c.indexSnapshot.Store(snapshot)
	c.scheduleNextRefresh()
	return snapshot, nil
}

func (c *DirectoryClient) scheduleNextRefresh() {
	if c.refreshInterval <= 0 {
		c.nextRefresh.Store(0)
		return
	}
	c.nextRefresh.Store(time.Now().Add(c.refreshInterval).UnixNano())
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
	return scoreDirectoryPageDetailed(page, terms, phrase, compactPhrase).baseScore
}

func scoreDirectoryPageDetailed(page directoryIndexPage, terms []string, phrase, compactPhrase string) directoryScoreExplanation {
	detail := directoryScoreExplanation{fieldScores: make(map[string]float64)}
	add := func(field string, score int) {
		detail.fieldScores[field] += float64(score)
		detail.baseScore += float64(score)
	}
	phraseMatch := func(name, field string, score int) {
		detail.phraseMatches = append(detail.phraseMatches, name)
		add(field, score)
	}
	if phrase != "" {
		if strings.Contains(page.titleText, phrase) {
			phraseMatch("title", "title", 100)
		}
		if strings.Contains(page.slugText, phrase) {
			phraseMatch("slug", "slug", 70)
		}
		if strings.Contains(page.headingText, phrase) {
			phraseMatch("heading", "heading", 40)
		}
		if strings.Contains(page.metadataText, phrase) {
			phraseMatch("metadata", "metadata", 35)
		}
		if strings.Contains(page.bodyText, phrase) {
			phraseMatch("body", "body", 20)
		}
	}
	// Compact matching bridges harmless punctuation and spacing differences,
	// especially mixed acronym/CJK queries such as "PBL历史旅行指南". Its
	// weights remain below normalized exact-phrase weights.
	if compactPhrase != "" {
		if strings.Contains(page.titleCompact, compactPhrase) {
			phraseMatch("compact_title", "title", 60)
		}
		if strings.Contains(page.slugCompact, compactPhrase) {
			phraseMatch("compact_slug", "slug", 40)
		}
		if strings.Contains(page.metadataCompact, compactPhrase) {
			phraseMatch("compact_metadata", "metadata", 20)
		}
		if strings.Contains(page.bodyCompact, compactPhrase) {
			phraseMatch("compact_body", "body", 10)
		}
	}
	matched := 0
	for _, term := range terms {
		titleScore := strings.Count(page.titleText, term) * 25
		slugScore := strings.Count(page.slugText, term) * 18
		headingScore := strings.Count(page.headingText, term) * 10
		metadataScore := strings.Count(page.metadataText, term) * 8
		bodyMatches := strings.Count(page.bodyText, term)
		if bodyMatches > 10 {
			bodyMatches = 10
		}
		bodyScore := bodyMatches * 2
		termScore := titleScore + slugScore + headingScore + metadataScore + bodyScore
		if termScore > 0 {
			matched++
			detail.matchedTerms = append(detail.matchedTerms, term)
			add("title", titleScore)
			add("slug", slugScore)
			add("heading", headingScore)
			add("metadata", metadataScore)
			add("body", bodyScore)
		}
	}
	if matched == len(terms) {
		add("all_terms", 25)
	}
	for field, score := range detail.fieldScores {
		if score == 0 {
			delete(detail.fieldScores, field)
		}
	}
	return detail
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
