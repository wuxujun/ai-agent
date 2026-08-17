// Package wiki adapts the read-only LLM Wiki MCP tools to typed documents.
package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/mcpclient"
)

const (
	searchToolName = "wiki_search"
	readToolName   = "wiki_content_read"
)

type Config struct {
	URL                 string
	Authorization       string
	Timeout             time.Duration
	AllowPrivateNetwork bool
}

// Document preserves stable Wiki identity and ranking metadata across the
// search-then-read flow.
type Document struct {
	Slug       string  `json:"slug,omitempty"`
	URI        string  `json:"uri"`
	Title      string  `json:"title,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	Excerpt    string  `json:"excerpt,omitempty"`
	Content    string  `json:"content,omitempty"`
	Status     string  `json:"status,omitempty"`
	Score      float64 `json:"score,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type Client struct {
	mcp          mcpCaller
	searchProps  map[string]any
	readProps    map[string]any
	graphProps   map[string]any
	suggestProps map[string]any
}

type mcpCaller interface {
	ListTools(context.Context) ([]mcpclient.Tool, error)
	CallTool(context.Context, string, map[string]any) (*mcpclient.CallResult, error)
	Close(context.Context) error
}

func New(cfg Config) (*Client, error) {
	client, err := mcpclient.New(mcpclient.Config{
		Name:                "llm-wiki",
		URL:                 cfg.URL,
		Authorization:       cfg.Authorization,
		Timeout:             cfg.Timeout,
		AllowPrivateNetwork: cfg.AllowPrivateNetwork,
	})
	if err != nil {
		return nil, err
	}
	return &Client{mcp: client}, nil
}

// Initialize discovers the server schemas and verifies that both required
// read-only tools exist. Arguments are filtered through these schemas so the
// adapter does not send provider-specific RAG fields to LLM Wiki.
func (c *Client) Initialize(ctx context.Context) error {
	if c == nil || c.mcp == nil {
		return errors.New("wiki client is nil")
	}
	remoteTools, err := c.mcp.ListTools(ctx)
	if err != nil {
		return err
	}
	for _, remote := range remoteTools {
		switch remote.Name {
		case searchToolName:
			c.searchProps = remote.Properties()
		case readToolName:
			c.readProps = remote.Properties()
		case graphToolName:
			c.graphProps = remote.Properties()
		case suggestToolName:
			c.suggestProps = remote.Properties()
		}
	}
	if c.searchProps == nil || c.readProps == nil {
		return fmt.Errorf("LLM Wiki must expose %s and %s", searchToolName, readToolName)
	}
	if !hasAnyProperty(c.searchProps, "query") {
		return fmt.Errorf("%s schema does not expose query", searchToolName)
	}
	if !hasAnyProperty(c.readProps, "uri", "slug", "path") {
		return fmt.Errorf("%s schema does not expose uri, slug, or path", readToolName)
	}
	return nil
}

func (c *Client) SupportsGraph() bool   { return c != nil && c.graphProps != nil }
func (c *Client) SupportsSuggest() bool { return c != nil && c.suggestProps != nil }

func (c *Client) Suggest(ctx context.Context, document Document, space string, limit int) (SuggestResult, error) {
	if !c.SupportsSuggest() {
		return SuggestResult{}, errors.New("remote Wiki does not expose wiki_suggest")
	}
	slug, space, limit, err := validateSuggestRequest(document, space, limit)
	if err != nil {
		return SuggestResult{}, err
	}
	uri := strings.TrimSpace(document.URI)
	if uri == "" {
		uri = graphURI(space, slug)
	}
	arguments := make(map[string]any)
	switch {
	case hasAnyProperty(c.suggestProps, "uri"):
		arguments["uri"] = uri
	case hasAnyProperty(c.suggestProps, "slug"):
		arguments["slug"] = slug
	case hasAnyProperty(c.suggestProps, "path"):
		arguments["path"] = slug
	default:
		return SuggestResult{}, errors.New("wiki_suggest schema does not expose uri, slug, or path")
	}
	setIfSupported(arguments, c.suggestProps, "wiki", space)
	setIfSupported(arguments, c.suggestProps, "limit", limit)
	setIfSupported(arguments, c.suggestProps, "format", "json")
	result, err := c.mcp.CallTool(ctx, suggestToolName, arguments)
	if err != nil {
		return SuggestResult{}, err
	}
	suggestions, err := parseSuggestResult(result)
	if err != nil {
		return SuggestResult{}, fmt.Errorf("parse %s result for %s: %w", suggestToolName, slug, err)
	}
	if suggestions.RootURI == "" {
		suggestions.RootURI = uri
	}
	return normalizeSuggestResult(suggestions, limit), nil
}

// Graph calls the optional read-only wiki_graph MCP operation. Its schema is
// discovered at startup and only supported arguments are sent.
func (c *Client) Graph(ctx context.Context, document Document, space string, depth int, direction string) (GraphResult, error) {
	if !c.SupportsGraph() {
		return GraphResult{}, errors.New("remote Wiki does not expose wiki_graph")
	}
	slug, direction, err := validateGraphRequest(document, space, depth, direction)
	if err != nil {
		return GraphResult{}, err
	}
	uri := strings.TrimSpace(document.URI)
	if uri == "" {
		uri = graphURI(strings.Trim(strings.TrimSpace(space), "/"), slug)
	}
	arguments := make(map[string]any)
	switch {
	case hasAnyProperty(c.graphProps, "uri"):
		arguments["uri"] = uri
	case hasAnyProperty(c.graphProps, "slug"):
		arguments["slug"] = slug
	case hasAnyProperty(c.graphProps, "path"):
		arguments["path"] = slug
	default:
		return GraphResult{}, errors.New("wiki_graph schema does not expose uri, slug, or path")
	}
	setIfSupported(arguments, c.graphProps, "wiki", strings.TrimSpace(space))
	setIfSupported(arguments, c.graphProps, "depth", depth)
	setIfSupported(arguments, c.graphProps, "direction", direction)
	result, err := c.mcp.CallTool(ctx, graphToolName, arguments)
	if err != nil {
		return GraphResult{}, err
	}
	graph, err := parseGraphResult(result)
	if err != nil {
		return GraphResult{}, fmt.Errorf("parse %s result for %s: %w", graphToolName, slug, err)
	}
	if graph.RootURI == "" {
		graph.RootURI = uri
	}
	return normalizeGraphResult(graph), nil
}

// Probe verifies that the remote Wiki still exposes the required read-only
// operations without mutating the schemas used by active tool calls.
func (c *Client) Probe(ctx context.Context) error {
	if c == nil || c.mcp == nil {
		return errors.New("wiki client is nil")
	}
	remoteTools, err := c.mcp.ListTools(ctx)
	if err != nil {
		return err
	}
	searchReady := false
	readReady := false
	for _, remote := range remoteTools {
		switch remote.Name {
		case searchToolName:
			searchReady = hasAnyProperty(remote.Properties(), "query")
		case readToolName:
			readReady = hasAnyProperty(remote.Properties(), "uri", "slug", "path")
		}
	}
	if !searchReady || !readReady {
		return fmt.Errorf("LLM Wiki must expose usable %s and %s tools", searchToolName, readToolName)
	}
	return nil
}

func (c *Client) Search(ctx context.Context, query string, topK int, space string) ([]Document, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("wiki search query must not be empty")
	}
	arguments := map[string]any{"query": strings.TrimSpace(query)}
	setIfSupported(arguments, c.searchProps, "top_k", topK)
	setIfSupported(arguments, c.searchProps, "format", "json")
	setIfSupported(arguments, c.searchProps, "wiki", strings.TrimSpace(space))
	result, err := c.mcp.CallTool(ctx, searchToolName, arguments)
	if err != nil {
		return nil, err
	}
	documents, err := parseSearchResult(result)
	if err != nil {
		return nil, fmt.Errorf("parse %s result: %w", searchToolName, err)
	}
	if topK > 0 && len(documents) > topK {
		documents = documents[:topK]
	}
	return documents, nil
}

func (c *Client) Read(ctx context.Context, document Document, space string) (Document, error) {
	uri := strings.TrimSpace(document.URI)
	slug := strings.TrimSpace(document.Slug)
	if slug == "" {
		slug = slugFromWikiURI(uri, space)
	}
	if uri == "" && slug == "" {
		return Document{}, errors.New("wiki document has no uri or slug")
	}
	arguments := make(map[string]any)
	switch {
	// Directory-backed LLM Wiki installations expose pages as relative slugs or
	// paths. Prefer those references and keep wiki:// URIs only as stable
	// evidence identities; they are not network URLs or filesystem paths.
	case hasAnyProperty(c.readProps, "slug") && slug != "":
		arguments["slug"] = slug
	case hasAnyProperty(c.readProps, "path") && slug != "":
		arguments["path"] = slug
	case hasAnyProperty(c.readProps, "uri") && uri != "":
		arguments["uri"] = uri
	case hasAnyProperty(c.readProps, "uri"):
		arguments["uri"] = slug
	default:
		return Document{}, errors.New("wiki document has no usable reference for the discovered read schema")
	}
	setIfSupported(arguments, c.readProps, "wiki", strings.TrimSpace(space))
	setIfSupported(arguments, c.readProps, "backlinks", true)
	setIfSupported(arguments, c.readProps, "format", "json")
	result, err := c.mcp.CallTool(ctx, readToolName, arguments)
	if err != nil {
		return Document{}, err
	}
	content, err := parseReadResult(result)
	if err != nil {
		return Document{}, fmt.Errorf("parse %s result for %s: %w", readToolName, firstNonEmpty(slug, uri), err)
	}
	document.Content = content
	return document, nil
}

func slugFromWikiURI(uri, space string) string {
	const prefix = "wiki://"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	value := strings.TrimPrefix(uri, prefix)
	if configuredSpace := strings.Trim(strings.TrimSpace(space), "/"); configuredSpace != "" {
		if rest, ok := strings.CutPrefix(value, configuredSpace+"/"); ok {
			return strings.Trim(rest, "/")
		}
	}
	_, rest, ok := strings.Cut(value, "/")
	if !ok {
		return ""
	}
	return strings.Trim(rest, "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (c *Client) Close(ctx context.Context) error {
	if c == nil || c.mcp == nil {
		return nil
	}
	return c.mcp.Close(ctx)
}

func setIfSupported(arguments, properties map[string]any, name string, value any) {
	if _, ok := properties[name]; !ok {
		return
	}
	if text, ok := value.(string); ok && text == "" {
		return
	}
	arguments[name] = value
}

func hasAnyProperty(properties map[string]any, names ...string) bool {
	for _, name := range names {
		if _, ok := properties[name]; ok {
			return true
		}
	}
	return false
}

func parseSearchResult(result *mcpclient.CallResult) ([]Document, error) {
	if result == nil {
		return nil, errors.New("empty MCP result")
	}
	for _, data := range [][]byte{result.StructuredContent, []byte(strings.TrimSpace(result.Text))} {
		if len(data) == 0 {
			continue
		}
		var envelope struct {
			Results []Document `json:"results"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil && envelope.Results != nil {
			return normalizeDocuments(envelope.Results), nil
		}
		var documents []Document
		if err := json.Unmarshal(data, &documents); err == nil && documents != nil {
			return normalizeDocuments(documents), nil
		}
	}
	return nil, errors.New("response did not contain a JSON results list")
}

func normalizeDocuments(documents []Document) []Document {
	result := make([]Document, 0, len(documents))
	for _, document := range documents {
		document.Slug = strings.TrimSpace(document.Slug)
		document.URI = strings.TrimSpace(document.URI)
		if document.URI == "" && document.Slug != "" {
			document.URI = document.Slug
		}
		if document.URI == "" {
			continue
		}
		result = append(result, document)
	}
	return result
}

func parseReadResult(result *mcpclient.CallResult) (string, error) {
	if result == nil {
		return "", errors.New("empty MCP result")
	}
	for _, data := range [][]byte{result.StructuredContent, []byte(strings.TrimSpace(result.Text))} {
		if len(data) == 0 {
			continue
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(data, &payload); err == nil && strings.TrimSpace(payload.Content) != "" {
			return strings.TrimSpace(payload.Content), nil
		}
	}
	if content := strings.TrimSpace(result.Text); content != "" {
		return content, nil
	}
	return "", errors.New("response did not contain page content")
}

func parseGraphResult(result *mcpclient.CallResult) (GraphResult, error) {
	if result == nil {
		return GraphResult{}, errors.New("empty MCP result")
	}
	for _, data := range [][]byte{result.StructuredContent, []byte(strings.TrimSpace(result.Text))} {
		if len(data) == 0 {
			continue
		}
		var envelope struct {
			Graph GraphResult `json:"graph"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil && (len(envelope.Graph.Nodes) > 0 || len(envelope.Graph.Edges) > 0) {
			return envelope.Graph, nil
		}
		var graph GraphResult
		if err := json.Unmarshal(data, &graph); err == nil && (len(graph.Nodes) > 0 || len(graph.Edges) > 0) {
			return graph, nil
		}
	}
	return GraphResult{}, errors.New("response did not contain graph nodes or edges")
}

func parseSuggestResult(result *mcpclient.CallResult) (SuggestResult, error) {
	if result == nil {
		return SuggestResult{}, errors.New("empty MCP result")
	}
	for _, data := range [][]byte{result.StructuredContent, []byte(strings.TrimSpace(result.Text))} {
		if len(data) == 0 {
			continue
		}
		var envelope SuggestResult
		if err := json.Unmarshal(data, &envelope); err == nil && envelope.Suggestions != nil {
			return envelope, nil
		}
		var suggestions []Suggestion
		if err := json.Unmarshal(data, &suggestions); err == nil && suggestions != nil {
			return SuggestResult{Suggestions: suggestions}, nil
		}
	}
	return SuggestResult{}, errors.New("response did not contain a JSON suggestions list")
}
