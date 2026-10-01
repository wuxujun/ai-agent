package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

// normalizeStepWorkspacePath accepts the common LLM form
// "<workspace>/<relative path>" while tools already execute relative to the
// task workspace. Removing that duplicate prefix prevents workspace/workspace
// lookups without weakening the tool layer's traversal checks.
func normalizeStepWorkspacePath(workspace string, step *ResearchStep) {
	if step == nil {
		return
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace != "." && workspace != "" && !filepath.IsAbs(workspace) && step.Action == "execute_code" {
		prefix := workspace + string(filepath.Separator)
		step.Args = strings.ReplaceAll(step.Args, "./"+prefix, "")
		step.Args = strings.ReplaceAll(step.Args, "'"+prefix, "'")
		step.Args = strings.ReplaceAll(step.Args, "\""+prefix, "\"")
		step.Args = strings.ReplaceAll(step.Args, " "+prefix, " ")
		step.Args = strings.TrimPrefix(step.Args, prefix)
		if step.RepairedParameters != nil {
			if _, exists := step.RepairedParameters["args"]; exists {
				step.RepairedParameters["args"] = step.Args
			}
		}
	}
	if strings.TrimSpace(step.FilePath) == "" || filepath.IsAbs(step.FilePath) {
		return
	}
	path := filepath.Clean(strings.TrimSpace(step.FilePath))
	if workspace == "." || workspace == "" || path == workspace {
		return
	}
	prefix := workspace + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return
	}
	relative := strings.TrimPrefix(path, prefix)
	if relative == "" || relative == "." {
		return
	}
	step.FilePath = relative
	if step.RepairedParameters != nil {
		if _, exists := step.RepairedParameters["path"]; exists {
			step.RepairedParameters["path"] = relative
		}
	}
}

// enforceJITResearchPlan prevents a multi-agent plan for an external factual
// lookup from drifting into workspace or execution tools before retrieval.
// Candidate detail steps are inserted later by retrievalFetchSteps.
func enforceJITResearchPlan(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || planner.HasSupportingEvidence(task.Trace) {
		return false
	}
	action, ok := planner.PreferredJITSearchAction(task)
	if !ok {
		return false
	}
	// If the preferred JIT search action has already been attempted, we should not
	// override the plan again. This allows the replanner to fallback to other tools
	// (like web_search) if RAG returned no results.
	for _, tr := range task.Trace {
		if tr.Action == action {
			return false
		}
	}
	if len(plan.Steps) == 1 && plan.Steps[0].Action == action {
		return false
	}
	plan.ThoughtSummary = "Retrieve authoritative evidence before factual synthesis"
	plan.Steps = []ResearchStep{{
		ID:          "step-1",
		Description: "Search the configured retrieval source for evidence",
		Action:      action,
		SearchQuery: task.Goal,
	}}
	return true
}

// enforceWorkspaceResearchPlan repairs the inverse of JIT drift: an explicit
// local workspace/repository goal planned entirely as external retrieval. It is
// intentionally narrow so mixed plans and legitimate RAG-first goals remain
// under planner control.
func enforceWorkspaceResearchPlan(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || !planner.GoalExplicitlyTargetsWorkspace(task.Goal) || len(plan.Steps) == 0 {
		return false
	}
	for _, step := range plan.Steps {
		switch step.Action {
		case "wiki_search", "wiki_fetch", "rag_search", "rag_fetch", "memory_search", "memory_get", "web_search", "http_fetch":
			// External-only plans are repaired below.
		default:
			return false
		}
	}
	plan.ThoughtSummary = "Discover and inspect relevant workspace files"
	plan.Steps = []ResearchStep{
		{ID: "step-1", Description: "Discover files in the task workspace", Action: "find_files", FileGlob: "*"},
		{ID: "step-2", Description: "Search workspace files for terms from the goal", Action: "search_text", SearchQuery: task.Goal},
	}
	return true
}

var explicitWorkspaceFilePattern = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]_./-])([[:alnum:]_.-]+\.(?:md|go|json|ya?ml|txt|csv))(?:$|[^[:alnum:]_./-])`)

// ensureExplicitWorkspaceFileReads prevents discovery-only plans from claiming
// to answer questions about a specifically named local file without reading it.
func ensureExplicitWorkspaceFileReads(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || !planner.GoalExplicitlyTargetsWorkspace(task.Goal) {
		return false
	}
	changed := false
	for _, match := range explicitWorkspaceFilePattern.FindAllStringSubmatch(task.Goal, -1) {
		name := filepath.Base(strings.TrimSpace(match[1]))
		if name == "." || name == "" {
			continue
		}
		found := false
		for _, step := range plan.Steps {
			if step.Action == "read_file" && filepath.Base(strings.TrimSpace(step.FilePath)) == name {
				found = true
				break
			}
		}
		if !found {
			for _, trace := range task.Trace {
				if trace.Action == "read_file" && strings.Contains(trace.Query, fmt.Sprintf("%q", name)) {
					found = true
					break
				}
			}
		}
		if found || (task.MaxSteps > 0 && len(plan.Steps) >= task.MaxSteps) {
			continue
		}
		plan.Steps = append(plan.Steps, ResearchStep{
			ID: fmt.Sprintf("step-%d", len(plan.Steps)+1), Description: "Read the explicitly named workspace file " + name,
			Action: "read_file", FilePath: name,
		})
		changed = true
	}
	return changed
}

func retrievalFetchSteps(evidence []StepEvidence) []ResearchStep {
	return retrievalFollowupSteps(context.Background(), evidence)
}

func retrievalFollowupSteps(ctx context.Context, evidence []StepEvidence) []ResearchStep {
	steps := make([]ResearchStep, 0, len(evidence))
	for _, item := range evidence {
		if item.Failed {
			continue
		}
		if item.Action == "wiki_fetch" && teamConfigFromContext(ctx).ActiveTeam == "wiki_graph" {
			for _, source := range item.Evidence {
				if strings.HasPrefix(source.Path, "wiki://") {
					steps = append(steps, ResearchStep{
						ID: item.StepID + "-graph", Description: "Read the bounded Wiki relationship graph", Action: "wiki_graph",
						GraphURI: source.Path, GraphDepth: 2, GraphDirection: "both",
					})
					break
				}
			}
			continue
		}
		if item.Action == "wiki_fetch" && teamConfigFromContext(ctx).ActiveTeam == "wiki_suggest" {
			for _, source := range item.Evidence {
				if strings.HasPrefix(source.Path, "wiki://") {
					steps = append(steps, ResearchStep{
						ID: item.StepID + "-suggest", Description: "Generate bounded read-only Wiki curation suggestions", Action: "wiki_suggest",
						SuggestURI: source.Path, SuggestLimit: 5,
					})
					break
				}
			}
			continue
		}
		if item.Action == "wiki_graph" && teamConfigFromContext(ctx).ActiveTeam == "wiki_graph" {
			uris := make([]string, 0, 3)
			for _, uri := range item.FollowupURIs {
				if !strings.HasPrefix(uri, "wiki://") || slices.Contains(uris, uri) {
					continue
				}
				uris = append(uris, uri)
				if len(uris) == 3 {
					break
				}
			}
			// Compatibility fallback for custom Researcher implementations that
			// have not adopted FollowupURIs. Built-in tools never depend on this
			// display Observation, which middleware is allowed to truncate.
			if len(uris) == 0 {
				var graph wiki.GraphResult
				if json.Unmarshal([]byte(item.Observation), &graph) == nil {
					for _, node := range graph.Nodes {
						if node.URI == graph.RootURI || !strings.HasPrefix(node.URI, "wiki://") {
							continue
						}
						uris = append(uris, node.URI)
						if len(uris) == 3 {
							break
						}
					}
				}
			}
			if len(uris) > 0 {
				steps = append(steps, ResearchStep{
					ID: item.StepID + "-fetch", Description: "Fetch selected bounded Wiki graph neighbor pages", Action: "wiki_graph_fetch",
					RepairedParameters: map[string]any{"uris": uris},
				})
			}
			continue
		}
		if item.Action != "wiki_search" && item.Action != "rag_search" && item.Action != "memory_search" {
			continue
		}
		var payload struct {
			Results []struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			} `json:"results"`
		}
		if json.Unmarshal([]byte(item.Observation), &payload) != nil {
			continue
		}
		limit := config.Get().RAG.JITFetchMaxItems
		if item.Action == "wiki_search" {
			limit = config.Get().Wiki.FetchMaxItems
		}
		if limit <= 0 {
			limit = 3
		}
		ids := make([]string, 0, min(limit, len(payload.Results)))
		if item.Action == "wiki_search" {
			ids = selectWikiFetchCandidateIDs(payload.Results, limit)
		} else {
			for _, candidate := range payload.Results {
				if candidate.ID != "" {
					ids = append(ids, candidate.ID)
				}
				if len(ids) >= limit {
					break
				}
			}
		}
		if len(ids) == 0 {
			continue
		}
		action := "rag_fetch"
		if item.Action == "memory_search" {
			action = "memory_get"
		} else if item.Action == "wiki_search" {
			action = "wiki_fetch"
		}
		steps = append(steps, ResearchStep{
			ID:                 item.StepID + "-fetch",
			Description:        "Fetch selected evidence from " + item.Action,
			Action:             action,
			RepairedParameters: map[string]any{"ids": ids},
		})
	}
	return steps
}

func selectWikiFetchCandidateIDs(candidates []struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}, limit int) []string {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	selected := make(map[string]bool, limit)
	categories := make(map[string]bool, limit)
	ids := make([]string, 0, min(limit, len(candidates)))
	add := func(candidateID, slug string) {
		if candidateID == "" || selected[candidateID] || len(ids) >= limit {
			return
		}
		selected[candidateID] = true
		categories[wikiSlugCategory(slug)] = true
		ids = append(ids, candidateID)
	}
	add(candidates[0].ID, candidates[0].Slug)
	for _, category := range []string{"sources", "entities", "comparisons", "concepts"} {
		if categories[category] || len(ids) >= limit {
			continue
		}
		for _, candidate := range candidates {
			if wikiSlugCategory(candidate.Slug) == category {
				add(candidate.ID, candidate.Slug)
				break
			}
		}
	}
	for _, candidate := range candidates {
		add(candidate.ID, candidate.Slug)
	}
	return ids
}

func wikiSlugCategory(slug string) string {
	category, _, found := strings.Cut(strings.Trim(strings.TrimSpace(slug), "/"), "/")
	if !found {
		return ""
	}
	return category
}
