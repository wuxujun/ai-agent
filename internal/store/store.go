package store

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/types"
)

// Store defines the interface for task, trace, and long-term memory storage.
type Store interface {
	// SaveFullTask persists a task and all its step traces atomically.
	SaveFullTask(ctx context.Context, task *types.Task) error

	// GetTask retrieves a task by ID. Returns sql.ErrNoRows if not found.
	GetTask(ctx context.Context, id string) (*types.Task, error)

	// ListTasks returns all tasks ordered by id. Implementations should apply a
	// reasonable upper limit (e.g. 500) to avoid unbounded memory usage.
	ListTasks(ctx context.Context) ([]*types.Task, error)

	// ExistsTask returns true if a task with the given id already exists.
	ExistsTask(ctx context.Context, id string) (bool, error)

	// SaveMemory persists a memory entry.
	SaveMemory(ctx context.Context, mem *types.Memory) error

	// QueryMemories retrieves the most relevant memories matching a query.
	// If embedding is provided, it can perform vector similarity search.
	QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error)

	// Close releases any resources held by the store.
	Close() error
}

