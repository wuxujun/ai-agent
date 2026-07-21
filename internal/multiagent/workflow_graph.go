package multiagent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WorkflowNodeCondition controls whether a node is activated after all of its
// dependencies have completed. Retries and replans remain node-local policies
// so that the orchestration topology stays acyclic.
type WorkflowNodeCondition string

const (
	WorkflowConditionAlways        WorkflowNodeCondition = "always"
	WorkflowConditionRouteResearch WorkflowNodeCondition = "route_research"
	WorkflowConditionRouteReviewed WorkflowNodeCondition = "route_reviewed"
	WorkflowConditionApproved      WorkflowNodeCondition = "approved"
)

type WorkflowGraphNode struct {
	ID        string                `json:"id"`
	Role      AgentRole             `json:"role"`
	DependsOn []string              `json:"depends_on,omitempty"`
	Condition WorkflowNodeCondition `json:"condition"`
}

type WorkflowGraph struct {
	Workflow Workflow            `json:"workflow"`
	Nodes    []WorkflowGraphNode `json:"nodes"`
}

type WorkflowGraphSummary struct {
	Digest           string
	NodeCount        int
	LevelCount       int
	MaxLevelWidth    int
	ConditionalNodes int
}

// BuildWorkflowGraph returns the validated built-in topology for a configured
// or effective workflow. Adaptive contains mutually exclusive route branches;
// the two effective workflows contain only the selected serial path.
func BuildWorkflowGraph(workflow Workflow) (WorkflowGraph, error) {
	var graph WorkflowGraph
	switch workflow {
	case WorkflowResearch:
		graph = WorkflowGraph{Workflow: workflow, Nodes: []WorkflowGraphNode{
			{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways},
			{ID: "research", Role: RoleResearcher, DependsOn: []string{"plan"}, Condition: WorkflowConditionAlways},
			{ID: "write", Role: RoleWriter, DependsOn: []string{"research"}, Condition: WorkflowConditionAlways},
		}}
	case WorkflowReviewed:
		graph = WorkflowGraph{Workflow: workflow, Nodes: []WorkflowGraphNode{
			{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways},
			{ID: "critique", Role: RoleCritic, DependsOn: []string{"plan"}, Condition: WorkflowConditionAlways},
			{ID: "execute", Role: RoleExecutor, DependsOn: []string{"critique"}, Condition: WorkflowConditionApproved},
			{ID: "verify", Role: RoleVerifier, DependsOn: []string{"execute"}, Condition: WorkflowConditionAlways},
		}}
	case WorkflowAdaptive:
		graph = WorkflowGraph{Workflow: workflow, Nodes: []WorkflowGraphNode{
			{ID: "plan", Role: RolePlanner, Condition: WorkflowConditionAlways},
			{ID: "research", Role: RoleResearcher, DependsOn: []string{"plan"}, Condition: WorkflowConditionRouteResearch},
			{ID: "write", Role: RoleWriter, DependsOn: []string{"research"}, Condition: WorkflowConditionAlways},
			{ID: "critique", Role: RoleCritic, DependsOn: []string{"plan"}, Condition: WorkflowConditionRouteReviewed},
			{ID: "execute", Role: RoleExecutor, DependsOn: []string{"critique"}, Condition: WorkflowConditionApproved},
			{ID: "verify", Role: RoleVerifier, DependsOn: []string{"execute"}, Condition: WorkflowConditionAlways},
		}}
	default:
		return WorkflowGraph{}, fmt.Errorf("unsupported multi-agent workflow %q", workflow)
	}
	if err := graph.Validate(); err != nil {
		return WorkflowGraph{}, fmt.Errorf("invalid built-in workflow graph %q: %w", workflow, err)
	}
	return graph, nil
}

func (g WorkflowGraph) Validate() error {
	if len(g.Nodes) == 0 {
		return fmt.Errorf("graph has no nodes")
	}
	knownRoles := map[AgentRole]bool{
		RolePlanner: true, RoleCritic: true, RoleExecutor: true,
		RoleVerifier: true, RoleResearcher: true, RoleWriter: true,
	}
	knownConditions := map[WorkflowNodeCondition]bool{
		WorkflowConditionAlways: true, WorkflowConditionRouteResearch: true,
		WorkflowConditionRouteReviewed: true, WorkflowConditionApproved: true,
	}
	nodes := make(map[string]WorkflowGraphNode, len(g.Nodes))
	rootCount := 0
	for _, node := range g.Nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			return fmt.Errorf("node id is empty")
		}
		if id != node.ID {
			return fmt.Errorf("node id %q has surrounding whitespace", node.ID)
		}
		if _, exists := nodes[id]; exists {
			return fmt.Errorf("node %q is duplicated", id)
		}
		if !knownRoles[node.Role] {
			return fmt.Errorf("node %q has unsupported role %q", id, node.Role)
		}
		if !knownConditions[node.Condition] {
			return fmt.Errorf("node %q has unsupported condition %q", id, node.Condition)
		}
		if len(node.DependsOn) == 0 {
			rootCount++
			if node.Condition != WorkflowConditionAlways {
				return fmt.Errorf("root node %q must use condition %q", id, WorkflowConditionAlways)
			}
		}
		nodes[id] = node
	}
	if rootCount != 1 {
		return fmt.Errorf("graph must have exactly one root, got %d", rootCount)
	}
	for _, node := range g.Nodes {
		dependencies := make(map[string]bool, len(node.DependsOn))
		for _, rawDependency := range node.DependsOn {
			dependency := strings.TrimSpace(rawDependency)
			if dependency == "" {
				return fmt.Errorf("node %q has an empty dependency", node.ID)
			}
			if dependency != rawDependency {
				return fmt.Errorf("node %q dependency %q has surrounding whitespace", node.ID, rawDependency)
			}
			if dependency == node.ID {
				return fmt.Errorf("node %q depends on itself", node.ID)
			}
			if dependencies[dependency] {
				return fmt.Errorf("node %q repeats dependency %q", node.ID, dependency)
			}
			dependencies[dependency] = true
			if _, exists := nodes[dependency]; !exists {
				return fmt.Errorf("node %q depends on unknown node %q", node.ID, dependency)
			}
		}
	}
	if _, err := g.topologicalLevels(); err != nil {
		return err
	}
	return nil
}

// TopologicalLevels returns deterministic execution-ready batches. Nodes in a
// level have no dependency on each other and may run concurrently when their
// activation conditions are compatible.
func (g WorkflowGraph) TopologicalLevels() ([][]WorkflowGraphNode, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g.topologicalLevels()
}

func (g WorkflowGraph) topologicalLevels() ([][]WorkflowGraphNode, error) {
	indegree := make(map[string]int, len(g.Nodes))
	dependents := make(map[string][]string, len(g.Nodes))
	byID := make(map[string]WorkflowGraphNode, len(g.Nodes))
	order := make(map[string]int, len(g.Nodes))
	for i, node := range g.Nodes {
		byID[node.ID] = node
		order[node.ID] = i
		indegree[node.ID] = len(node.DependsOn)
		for _, dependency := range node.DependsOn {
			dependents[dependency] = append(dependents[dependency], node.ID)
		}
	}
	ready := make([]string, 0, len(g.Nodes))
	for _, node := range g.Nodes {
		if indegree[node.ID] == 0 {
			ready = append(ready, node.ID)
		}
	}
	levels := make([][]WorkflowGraphNode, 0, len(g.Nodes))
	visited := 0
	for len(ready) > 0 {
		levelIDs := append([]string(nil), ready...)
		sort.SliceStable(levelIDs, func(i, j int) bool { return order[levelIDs[i]] < order[levelIDs[j]] })
		ready = ready[:0]
		level := make([]WorkflowGraphNode, 0, len(levelIDs))
		for _, id := range levelIDs {
			level = append(level, byID[id])
			visited++
			for _, dependent := range dependents[id] {
				indegree[dependent]--
				if indegree[dependent] == 0 {
					ready = append(ready, dependent)
				}
			}
		}
		levels = append(levels, level)
	}
	if visited != len(g.Nodes) {
		return nil, fmt.Errorf("graph contains a dependency cycle")
	}
	return levels, nil
}

func (g WorkflowGraph) Summary() (WorkflowGraphSummary, error) {
	levels, err := g.TopologicalLevels()
	if err != nil {
		return WorkflowGraphSummary{}, err
	}
	canonical := WorkflowGraph{Workflow: g.Workflow, Nodes: append([]WorkflowGraphNode(nil), g.Nodes...)}
	sort.Slice(canonical.Nodes, func(i, j int) bool { return canonical.Nodes[i].ID < canonical.Nodes[j].ID })
	for i := range canonical.Nodes {
		canonical.Nodes[i].DependsOn = append([]string(nil), canonical.Nodes[i].DependsOn...)
		sort.Strings(canonical.Nodes[i].DependsOn)
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return WorkflowGraphSummary{}, fmt.Errorf("marshal workflow graph: %w", err)
	}
	digest := sha256.Sum256(raw)
	summary := WorkflowGraphSummary{
		Digest:     fmt.Sprintf("%x", digest[:12]),
		NodeCount:  len(g.Nodes),
		LevelCount: len(levels),
	}
	for _, node := range g.Nodes {
		if node.Condition != WorkflowConditionAlways {
			summary.ConditionalNodes++
		}
	}
	for _, level := range levels {
		if len(level) > summary.MaxLevelWidth {
			summary.MaxLevelWidth = len(level)
		}
	}
	return summary, nil
}
