package wiki

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const graphToolName = "wiki_graph"

const (
	maxGraphNodes = 100
	maxGraphEdges = 250
)

type GraphNode struct {
	URI     string `json:"uri"`
	Slug    string `json:"slug,omitempty"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type GraphResult struct {
	RootURI string      `json:"root_uri"`
	Nodes   []GraphNode `json:"nodes"`
	Edges   []GraphEdge `json:"edges"`
}

func validateGraphRequest(document Document, space string, depth int, direction string) (string, string, error) {
	space = strings.Trim(strings.TrimSpace(space), "/")
	if space == "" {
		if parsed, err := url.Parse(strings.TrimSpace(document.URI)); err == nil && parsed.Scheme == "wiki" {
			space = strings.TrimSpace(parsed.Host)
		}
		if space == "" {
			return "", "", errors.New("wiki graph space must not be empty")
		}
	}
	slug := strings.Trim(strings.TrimSpace(document.Slug), "/")
	if slug == "" {
		slug = slugFromWikiURI(strings.TrimSpace(document.URI), space)
	}
	if slug == "" || strings.Contains(slug, "..") || strings.Contains(slug, "\\") {
		return "", "", errors.New("wiki graph document must have a safe slug in the requested space")
	}
	if uri := strings.TrimSpace(document.URI); uri != "" && !strings.HasPrefix(uri, "wiki://"+space+"/") {
		return "", "", errors.New("wiki graph URI does not belong to the requested space")
	}
	if depth < 1 || depth > 2 {
		return "", "", errors.New("wiki graph depth must be 1 or 2")
	}
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "both"
	}
	if direction != "outgoing" && direction != "incoming" && direction != "both" {
		return "", "", errors.New("wiki graph direction must be outgoing, incoming, or both")
	}
	return slug, direction, nil
}

func normalizeGraphResult(result GraphResult) GraphResult {
	nodes := make(map[string]GraphNode, len(result.Nodes))
	for _, node := range result.Nodes {
		if node.URI = strings.TrimSpace(node.URI); node.URI != "" {
			nodes[node.URI] = node
		}
	}
	edges := make(map[string]GraphEdge, len(result.Edges))
	for _, edge := range result.Edges {
		edge.From = strings.TrimSpace(edge.From)
		edge.To = strings.TrimSpace(edge.To)
		if edge.From != "" && edge.To != "" {
			edges[edge.From+"\x00"+edge.To] = edge
		}
	}
	result.Nodes = result.Nodes[:0]
	for _, node := range nodes {
		result.Nodes = append(result.Nodes, node)
	}
	result.Edges = result.Edges[:0]
	for _, edge := range edges {
		result.Edges = append(result.Edges, edge)
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].URI < result.Nodes[j].URI })
	sort.Slice(result.Edges, func(i, j int) bool {
		if result.Edges[i].From == result.Edges[j].From {
			return result.Edges[i].To < result.Edges[j].To
		}
		return result.Edges[i].From < result.Edges[j].From
	})
	if len(result.Nodes) > maxGraphNodes {
		rootIndex := -1
		for index := range result.Nodes {
			if result.Nodes[index].URI == result.RootURI {
				rootIndex = index
				break
			}
		}
		result.Nodes = result.Nodes[:maxGraphNodes]
		if rootIndex >= maxGraphNodes {
			result.Nodes[maxGraphNodes-1] = nodes[result.RootURI]
			sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].URI < result.Nodes[j].URI })
		}
	}
	allowedNodes := make(map[string]bool, len(result.Nodes))
	for _, node := range result.Nodes {
		allowedNodes[node.URI] = true
	}
	filteredEdges := result.Edges[:0]
	for _, edge := range result.Edges {
		if allowedNodes[edge.From] && allowedNodes[edge.To] {
			filteredEdges = append(filteredEdges, edge)
			if len(filteredEdges) == maxGraphEdges {
				break
			}
		}
	}
	result.Edges = filteredEdges
	return result
}

func graphURI(space, slug string) string { return fmt.Sprintf("wiki://%s/%s", space, slug) }
