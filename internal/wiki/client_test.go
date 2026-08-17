package wiki

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/mcpclient"
)

type fakeMCP struct {
	tools []mcpclient.Tool
	calls []fakeMCPCall
}

type fakeMCPCall struct {
	name string
	args map[string]any
}

func (f *fakeMCP) ListTools(context.Context) ([]mcpclient.Tool, error) { return f.tools, nil }
func (f *fakeMCP) Close(context.Context) error                         { return nil }
func (f *fakeMCP) CallTool(_ context.Context, name string, args map[string]any) (*mcpclient.CallResult, error) {
	f.calls = append(f.calls, fakeMCPCall{name: name, args: args})
	if name == searchToolName {
		return &mcpclient.CallResult{StructuredContent: json.RawMessage(`{"results":[{"slug":"concepts/moe","uri":"wiki://research/concepts/moe","title":"MoE","summary":"summary","excerpt":"excerpt","score":0.94,"confidence":0.9}]}`)}, nil
	}
	if name == graphToolName {
		return &mcpclient.CallResult{StructuredContent: json.RawMessage(`{"root_uri":"wiki://research/concepts/moe","nodes":[{"uri":"wiki://research/concepts/moe"},{"uri":"wiki://research/sources/moe"}],"edges":[{"from":"wiki://research/concepts/moe","to":"wiki://research/sources/moe"}]}`)}, nil
	}
	if name == suggestToolName {
		return &mcpclient.CallResult{StructuredContent: json.RawMessage(`{"root_uri":"wiki://research/concepts/moe","suggestions":[{"kind":"missing_link","uri":"wiki://research/sources/moe","reason":"relevant but unlinked","score":0.9}]}`)}, nil
	}
	return &mcpclient.CallResult{Text: `{"content":"# MoE\n\nFull page"}`}, nil
}

func wikiTestTools() []mcpclient.Tool {
	return []mcpclient.Tool{
		{Name: searchToolName, InputSchema: map[string]any{"properties": map[string]any{
			"query": map[string]any{"type": "string"}, "top_k": map[string]any{"type": "integer"}, "format": map[string]any{"type": "string"}, "wiki": map[string]any{"type": "string"},
		}}},
		{Name: readToolName, InputSchema: map[string]any{"properties": map[string]any{
			"uri": map[string]any{"type": "string"}, "wiki": map[string]any{"type": "string"}, "backlinks": map[string]any{"type": "boolean"}, "format": map[string]any{"type": "string"},
		}}},
	}
}

func TestClientProbeRequiresUsableReadOnlyTools(t *testing.T) {
	client := &Client{mcp: &fakeMCP{tools: wikiTestTools()}}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatalf("healthy probe: %v", err)
	}
	client.mcp = &fakeMCP{tools: []mcpclient.Tool{{Name: searchToolName, InputSchema: map[string]any{
		"properties": map[string]any{"query": map[string]any{"type": "string"}},
	}}}}
	if err := client.Probe(t.Context()); err == nil {
		t.Fatal("probe succeeded without the read tool")
	}
}

func TestClientSearchThenReadPreservesWikiIdentity(t *testing.T) {
	transport := &fakeMCP{tools: wikiTestTools()}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "mixture of experts", 3, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].URI != "wiki://research/concepts/moe" || documents[0].Score != 0.94 || documents[0].Confidence != 0.9 {
		t.Fatalf("documents = %+v", documents)
	}
	page, err := client.Read(t.Context(), documents[0], "research")
	if err != nil {
		t.Fatal(err)
	}
	if page.Content != "# MoE\n\nFull page" || page.URI != documents[0].URI {
		t.Fatalf("page = %+v", page)
	}
	if len(transport.calls) != 2 || transport.calls[0].name != searchToolName || transport.calls[1].name != readToolName {
		t.Fatalf("calls = %+v", transport.calls)
	}
	if got := transport.calls[0].args; !reflect.DeepEqual(got, map[string]any{"query": "mixture of experts", "top_k": 3, "format": "json", "wiki": "research"}) {
		t.Fatalf("search args = %#v", got)
	}
	if got := transport.calls[1].args; !reflect.DeepEqual(got, map[string]any{"uri": documents[0].URI, "wiki": "research", "backlinks": true, "format": "json"}) {
		t.Fatalf("read args = %#v", got)
	}
}

func TestClientFiltersArgumentsUsingDiscoveredSchema(t *testing.T) {
	transport := &fakeMCP{tools: []mcpclient.Tool{
		{Name: searchToolName, InputSchema: map[string]any{"properties": map[string]any{"query": map[string]any{"type": "string"}}}},
		{Name: readToolName, InputSchema: map[string]any{"properties": map[string]any{"slug": map[string]any{"type": "string"}}}},
	}}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(t.Context(), "query", 5, "private-space")
	if err != nil {
		t.Fatal(err)
	}
	if got := transport.calls[0].args; !reflect.DeepEqual(got, map[string]any{"query": "query"}) {
		t.Fatalf("unsupported arguments leaked: %#v", got)
	}
	if _, err := client.Read(t.Context(), documents[0], "private-space"); err != nil {
		t.Fatal(err)
	}
	if got := transport.calls[1].args; !reflect.DeepEqual(got, map[string]any{"slug": documents[0].Slug}) {
		t.Fatalf("read args = %#v", got)
	}
}

func TestClientPrefersDirectoryPathOverWikiURI(t *testing.T) {
	transport := &fakeMCP{tools: []mcpclient.Tool{
		{Name: searchToolName, InputSchema: map[string]any{"properties": map[string]any{"query": map[string]any{"type": "string"}}}},
		{Name: readToolName, InputSchema: map[string]any{"properties": map[string]any{
			"uri": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
		}}},
	}}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	document := Document{URI: "wiki://research/concepts/moe", Slug: "concepts/moe"}
	if _, err := client.Read(t.Context(), document, "research"); err != nil {
		t.Fatal(err)
	}
	if got := transport.calls[0].args; !reflect.DeepEqual(got, map[string]any{"path": "concepts/moe"}) {
		t.Fatalf("directory read args = %#v", got)
	}
}

func TestClientDerivesDirectoryPathFromWikiURI(t *testing.T) {
	transport := &fakeMCP{tools: []mcpclient.Tool{
		{Name: searchToolName, InputSchema: map[string]any{"properties": map[string]any{"query": map[string]any{"type": "string"}}}},
		{Name: readToolName, InputSchema: map[string]any{"properties": map[string]any{"path": map[string]any{"type": "string"}}}},
	}}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(t.Context(), Document{URI: "wiki://research/comparisons/agents"}, "research"); err != nil {
		t.Fatal(err)
	}
	if got := transport.calls[0].args["path"]; got != "comparisons/agents" {
		t.Fatalf("derived path = %v", got)
	}
}

func TestInitializeRequiresReadOnlyTools(t *testing.T) {
	client := &Client{mcp: &fakeMCP{tools: wikiTestTools()[:1]}}
	if err := client.Initialize(t.Context()); err == nil {
		t.Fatal("expected missing wiki_content_read to fail initialization")
	}
}

func TestClientUsesOptionalWikiGraphSchema(t *testing.T) {
	transport := &fakeMCP{tools: append(wikiTestTools(), mcpclient.Tool{Name: graphToolName, InputSchema: map[string]any{"properties": map[string]any{
		"uri": map[string]any{"type": "string"}, "wiki": map[string]any{"type": "string"}, "depth": map[string]any{"type": "integer"}, "direction": map[string]any{"type": "string"},
	}}})}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !client.SupportsGraph() {
		t.Fatal("optional wiki_graph was not discovered")
	}
	graph, err := client.Graph(t.Context(), Document{URI: "wiki://research/concepts/moe"}, "research", 2, "outgoing")
	if err != nil || len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("graph=%+v err=%v", graph, err)
	}
	if got := transport.calls[0].args; !reflect.DeepEqual(got, map[string]any{"uri": "wiki://research/concepts/moe", "wiki": "research", "depth": 2, "direction": "outgoing"}) {
		t.Fatalf("graph args = %#v", got)
	}
}

func TestClientUsesOptionalWikiSuggestSchema(t *testing.T) {
	transport := &fakeMCP{tools: append(wikiTestTools(), mcpclient.Tool{Name: suggestToolName, InputSchema: map[string]any{"properties": map[string]any{
		"uri": map[string]any{"type": "string"}, "wiki": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}, "format": map[string]any{"type": "string"},
	}}})}
	client := &Client{mcp: transport}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !client.SupportsSuggest() {
		t.Fatal("optional wiki_suggest was not discovered")
	}
	result, err := client.Suggest(t.Context(), Document{URI: "wiki://research/concepts/moe"}, "research", 4)
	if err != nil || len(result.Suggestions) != 1 || result.Suggestions[0].Kind != "missing_link" {
		t.Fatalf("suggestions=%+v err=%v", result, err)
	}
	if got := transport.calls[0].args; !reflect.DeepEqual(got, map[string]any{"uri": "wiki://research/concepts/moe", "wiki": "research", "limit": 4, "format": "json"}) {
		t.Fatalf("suggest args=%#v", got)
	}
}
