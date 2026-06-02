package tools

import (
	"context"
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

func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list []Tool
	for _, t := range r.tools {
		list = append(list, t)
	}
	return list
}

var DefaultRegistry = NewRegistry()

func Register(t Tool) {
	DefaultRegistry.Register(t)
}

func Get(name string) (Tool, bool) {
	return DefaultRegistry.Get(name)
}
