package store

import (
	"context"
	"database/sql"
	"sync"

	"github.com/wuxujun/ai-agent/internal/types"
)

// MemoryStore implements Store in-memory for testing or ephemeral executions.
type MemoryStore struct {
	mu    sync.RWMutex
	tasks map[string]*types.Task
}

// NewMemoryStore initializes a new MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks: make(map[string]*types.Task),
	}
}

// SaveFullTask saves or updates a task and its traces in memory.
func (m *MemoryStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clone to avoid concurrent mutation issues
	cloned := *task
	if task.Unresolved != nil {
		cloned.Unresolved = make([]string, len(task.Unresolved))
		copy(cloned.Unresolved, task.Unresolved)
	}
	if task.Trace != nil {
		cloned.Trace = make([]types.StepTrace, len(task.Trace))
		copy(cloned.Trace, task.Trace)
	}

	m.tasks[task.ID] = &cloned
	return nil
}

// GetTask retrieves a task from memory. Returns sql.ErrNoRows if not found.
func (m *MemoryStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	task, exists := m.tasks[id]
	if !exists {
		return nil, sql.ErrNoRows
	}

	// Return a clone
	cloned := *task
	if task.Unresolved != nil {
		cloned.Unresolved = make([]string, len(task.Unresolved))
		copy(cloned.Unresolved, task.Unresolved)
	}
	if task.Trace != nil {
		cloned.Trace = make([]types.StepTrace, len(task.Trace))
		copy(cloned.Trace, task.Trace)
	}

	return &cloned, nil
}

// Close is a no-op for MemoryStore.
func (m *MemoryStore) Close() error {
	return nil
}
