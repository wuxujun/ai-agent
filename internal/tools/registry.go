package tools

import (
	"context"
	"sort"
	"sync"

	"github.com/wuxujun/ai-agent/internal/types"
)

type ToolResult struct {
	Query       string
	Observation string
	Evidence    []types.Evidence
}

type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON-Schema fragment describing this tool's input
	// parameters, keyed by parameter name (e.g. {"path": {"type": "string"}}).
	// The planner schema is assembled by merging the Parameters() of every
	// registered tool, so adding a tool no longer requires hand-editing the
	// planner schema.
	Parameters() map[string]any
	Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error)
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns all registered tools ordered by name. Deterministic ordering
// keeps the generated planner schema and prompt tool list stable across runs.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name() < list[j].Name()
	})
	return list
}

// Names returns the sorted names of all registered tools.
func (r *Registry) Names() []string {
	list := r.List()
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Name()
	}
	return names
}

var DefaultRegistry = NewRegistry()

// Names returns the sorted names of all tools in the default registry.
func Names() []string {
	return DefaultRegistry.Names()
}

func Register(t Tool) {
	DefaultRegistry.Register(t)
}

func Get(name string) (Tool, bool) {
	return DefaultRegistry.Get(name)
}
