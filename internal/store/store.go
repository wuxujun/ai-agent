package store

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/types"
)

// ListFilter controls the result set returned by ListTasks.
// Zero values are safe: an empty Status means "all statuses", Limit=0 is
// coerced by each implementation to its own default (50), and Offset=0 means
// the first page.
type ListFilter struct {
	// Status, if non-empty, restricts results to tasks with this status
	// (e.g. "created", "running", "completed", "failed").
	Status types.TaskStatus

	// Limit is the maximum number of tasks to return. 0 → implementation default (50).
	// The absolute maximum accepted by any implementation is 500.
	Limit int

	// Offset is the number of tasks to skip (cursor-style pagination).
	Offset int
}

// Store defines the interface for task, trace, and long-term memory storage.
type Store interface {
	// SaveFullTask persists a task and all its step traces atomically.
	SaveFullTask(ctx context.Context, task *types.Task) error

	// GetTask retrieves a task by ID. Returns sql.ErrNoRows if not found.
	GetTask(ctx context.Context, id string) (*types.Task, error)

	// ListTasks returns tasks matching f, ordered by id ASC.
	// Callers should use ListFilter{} (zero value) to list all tasks with the
	// default page size. Pagination is achieved by incrementing f.Offset.
	ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error)

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

// resolveLimit normalises the caller-supplied limit: 0 → defaultLimit,
// above maxLimit → maxLimit.
func resolveLimit(limit, defaultLimit, maxLimit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
