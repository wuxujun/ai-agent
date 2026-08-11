package store

import (
	"context"
	"errors"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type tenantScopeContextKey struct{}
type sessionScopeContextKey struct{}

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionArchived = errors.New("session is archived")
)

func WithTenantScope(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantScopeContextKey{}, tenantID)
}

func tenantScope(ctx context.Context) (string, bool) {
	tenantID, ok := ctx.Value(tenantScopeContextKey{}).(string)
	return tenantID, ok
}

// WithSessionScope restricts memory retrieval to one session. Tenant scope
// remains mandatory and is applied independently by each backend.
func WithSessionScope(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionScopeContextKey{}, sessionID)
}

func sessionScope(ctx context.Context) (string, bool) {
	sessionID, ok := ctx.Value(sessionScopeContextKey{}).(string)
	return sessionID, ok && sessionID != ""
}

func memoryTenantMatches(scope, tenantID string) bool {
	return tenantID == scope || (scope == "default" && tenantID == "")
}

// ListFilter controls the result set returned by ListTasks.
// Zero values are safe: an empty Status means "all statuses", Limit=0 is
// coerced by each implementation to its own default (50), and Offset=0 means
// the first page.
type ListFilter struct {
	TenantID  string
	SessionID string
	// Status, if non-empty, restricts results to tasks with this status
	// (e.g. "created", "running", "completed", "failed").
	Status types.TaskStatus

	// Limit is the maximum number of tasks to return. 0 → implementation default (50).
	// The absolute maximum accepted by any implementation is 500.
	Limit int

	// Offset is the number of tasks to skip (cursor-style pagination).
	Offset int
}

type ListMemoryFilter struct {
	TenantID  string
	SessionID string
	Limit     int
	Offset    int
}

type ListSessionFilter struct {
	TenantID string
	Status   types.SessionStatus
	Limit    int
	Offset   int
}

// SessionStore is implemented by persistent stores that support multi-task
// conversations. Sequence allocation must be atomic across instances.
type SessionStore interface {
	CreateSession(ctx context.Context, session *types.Session) error
	GetSession(ctx context.Context, id, tenantID string) (*types.Session, error)
	ListSessions(ctx context.Context, filter ListSessionFilter) ([]*types.Session, error)
	UpdateSession(ctx context.Context, session *types.Session) error
	NextSessionTaskSequence(ctx context.Context, id, tenantID string) (int64, error)
}

// Store defines the interface for task, trace, and long-term memory storage.
type Store interface {
	// SaveFullTask persists a task and all its step traces atomically.
	SaveFullTask(ctx context.Context, task *types.Task) error

	// GetTask retrieves a task by ID. Returns sql.ErrNoRows if not found.
	GetTask(ctx context.Context, id string) (*types.Task, error)

	// ListTasks returns tasks matching f. Session-filtered results are ordered by
	// sequence number; other results retain the backend's stable task ordering.
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

// TaskDeletionStore is implemented by stores that can remove persisted tasks.
// Keeping deletion separate preserves compatibility with read/write Store
// adapters that do not provide destructive administration operations.
type TaskDeletionStore interface {
	DeleteTask(ctx context.Context, id string) (bool, error)
	DeleteAllTasks(ctx context.Context) (int64, error)
}

// DurableApprovalStore persists approval state independently from Task JSON.
// Implementations must enforce tenant scope, versioned CAS transitions, and
// owner-checked leases. Payload bytes are ciphertext produced above Store.
type DurableApprovalStore interface {
	CreateApproval(ctx context.Context, approval *types.DurableApproval) error
	GetApproval(ctx context.Context, id, tenantID string) (*types.DurableApproval, error)
	ListTaskApprovals(ctx context.Context, taskID, tenantID string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error)
	TransitionApproval(ctx context.Context, id, tenantID string, expectedVersion int64, from, to types.DurableApprovalStatus, resolutionPayload []byte) (bool, error)
	AcquireApprovalLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error)
	ReleaseApprovalLease(ctx context.Context, id, owner string) error
}

// MemoryManagementStore provides administrative listing and deletion without
// expanding the core Store contract used by lightweight adapters.
type MemoryManagementStore interface {
	ListMemories(ctx context.Context, filter ListMemoryFilter) ([]*types.Memory, error)
	DeleteMemory(ctx context.Context, id, tenantID string) (bool, error)
	DeleteAllMemories(ctx context.Context, tenantID string) (int64, error)
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
