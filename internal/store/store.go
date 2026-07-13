package store

import (
	"context"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type tenantScopeContextKey struct{}

func WithTenantScope(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantScopeContextKey{}, tenantID)
}

func tenantScope(ctx context.Context) (string, bool) {
	tenantID, ok := ctx.Value(tenantScopeContextKey{}).(string)
	return tenantID, ok
}

func memoryTenantMatches(scope, tenantID string) bool {
	return tenantID == scope || (scope == "default" && tenantID == "")
}

// ListFilter controls the result set returned by ListTasks.
// Zero values are safe: an empty Status means "all statuses", Limit=0 is
// coerced by each implementation to its own default (50), and Offset=0 means
// the first page.
type ListFilter struct {
	TenantID string
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

	// TryTransitionTaskStatus atomically attempts to transition a task's status from one of the allowed 'from' statuses to a target status.
	// It returns (true, nil) if the transition succeeded, or (false, nil) if the status did not match.
	TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error)

	// AcquireTaskLease obtains or renews an execution lease for task id.
	// It succeeds when no lease exists, the existing lease expired, or owner
	// already holds it. Leases prevent the same task executing on two instances.
	AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error)

	// ReleaseTaskLease removes a lease only when it is still owned by owner.
	ReleaseTaskLease(ctx context.Context, id, owner string) error

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
