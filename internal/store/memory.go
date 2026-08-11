package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
)

// MemoryStore implements Store in-memory for testing or ephemeral executions.
type MemoryStore struct {
	mu          sync.RWMutex
	tasks       map[string]*types.Task
	sessions    map[string]*types.Session
	memories    map[string]*types.Memory
	leases      map[string]memoryLease
	approvals   map[string]*types.DurableApproval
	indexing    map[string]bool // Tracks tasks currently undergoing async indexing
	tenantUsage map[string]types.TenantLLMUsage
}

type memoryLease struct {
	owner     string
	expiresAt time.Time
}

// NewMemoryStore initializes a new MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:       make(map[string]*types.Task),
		sessions:    make(map[string]*types.Session),
		memories:    make(map[string]*types.Memory),
		leases:      make(map[string]memoryLease),
		approvals:   make(map[string]*types.DurableApproval),
		indexing:    make(map[string]bool),
		tenantUsage: make(map[string]types.TenantLLMUsage),
	}
}

func tenantUsageKey(tenantID string, periodStart time.Time) string {
	return tenantID + ":" + periodStart.UTC().Format("2006-01-02")
}

func (m *MemoryStore) ReserveTenantLLMCall(_ context.Context, tenantID string, periodStart time.Time, budget types.TenantLLMBudget) (types.TenantLLMUsage, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenantUsage == nil {
		m.tenantUsage = make(map[string]types.TenantLLMUsage)
	}
	key := tenantUsageKey(tenantID, periodStart)
	usage := m.tenantUsage[key]
	if (budget.MaxCalls > 0 && usage.Calls >= budget.MaxCalls) || (budget.MaxEstimatedCostUSD > 0 && usage.EstimatedCostUSD >= budget.MaxEstimatedCostUSD) {
		return usage, false, nil
	}
	usage.Calls++
	m.tenantUsage[key] = usage
	return usage, true, nil
}

func (m *MemoryStore) AddTenantLLMCost(_ context.Context, tenantID string, periodStart time.Time, estimatedCostUSD float64) error {
	if estimatedCostUSD <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenantUsage == nil {
		m.tenantUsage = make(map[string]types.TenantLLMUsage)
	}
	key := tenantUsageKey(tenantID, periodStart)
	usage := m.tenantUsage[key]
	usage.EstimatedCostUSD += estimatedCostUSD
	m.tenantUsage[key] = usage
	return nil
}

func (m *MemoryStore) GetTenantLLMUsage(_ context.Context, tenantID string, periodStart time.Time) (types.TenantLLMUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tenantUsage[tenantUsageKey(tenantID, periodStart)], nil
}

// SaveFullTask saves or updates a task and its traces in memory.
func (m *MemoryStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	m.mu.Lock()
	if m.indexing == nil {
		m.indexing = make(map[string]bool)
	}
	_, alreadyIndexed := m.memories[memory.TaskMemoryID(task)]
	alreadyIndexing := m.indexing[task.ID]

	cloned := types.CloneTask(task)

	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
		cloned.CreatedAt = task.CreatedAt
	}
	task.UpdatedAt = time.Now().UTC()
	cloned.UpdatedAt = task.UpdatedAt
	m.tasks[task.ID] = cloned
	if session := m.sessions[task.SessionID]; session != nil {
		session.UpdatedAt = task.UpdatedAt
	}

	shouldIndex := memory.ShouldIndexTask(task) && !alreadyIndexed && !alreadyIndexing
	if shouldIndex {
		m.indexing[task.ID] = true
	}
	m.mu.Unlock()

	if shouldIndex {
		// Asynchronously index completed task as a long-term memory for cross-task RAG.
		// Since generating embeddings can take time (e.g. hitting remote APIs),
		// we run this outside of the write lock to prevent blocking memory storage.
		go func() {
			defer func() {
				m.mu.Lock()
				delete(m.indexing, cloned.ID)
				m.mu.Unlock()
			}()

			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			mem, err := memory.CreateMemoryFromTask(bgCtx, cloned)
			if err != nil {
				log.Warn("failed to create memory from completed task in memory store", "task_id", cloned.ID, "error", err)
				return
			}

			m.mu.Lock()
			clonedMem := *mem
			if mem.Embedding != nil {
				clonedMem.Embedding = make([]float32, len(mem.Embedding))
				copy(clonedMem.Embedding, mem.Embedding)
			}
			m.memories[mem.ID] = &clonedMem
			m.mu.Unlock()
		}()
	}

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

	return types.CloneTask(task), nil
}

// Close is a no-op for MemoryStore.
func (m *MemoryStore) Close() error {
	return nil
}

// ListTasks returns tasks matching f. MemoryStore applies status filter and pagination in-process.
func (m *MemoryStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tasks := make([]*types.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if f.TenantID != "" && t.TenantID != f.TenantID {
			continue
		}
		if f.SessionID != "" && t.SessionID != f.SessionID {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		tasks = append(tasks, types.CloneTask(t))
	}
	sort.Slice(tasks, func(i, j int) bool {
		if f.SessionID != "" && tasks[i].SequenceNo != tasks[j].SequenceNo {
			return tasks[i].SequenceNo < tasks[j].SequenceNo
		}
		return tasks[i].ID < tasks[j].ID
	})

	// Apply pagination
	limit := resolveLimit(f.Limit, 50, 500)
	offset := f.Offset
	if offset >= len(tasks) {
		return []*types.Task{}, nil
	}
	tasks = tasks[offset:]
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, nil
}

// ExistsTask returns true if a task with the given id already exists.
func (m *MemoryStore) ExistsTask(ctx context.Context, id string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.tasks[id]
	return exists, nil
}

func (m *MemoryStore) DeleteTask(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[id]; !exists {
		return false, nil
	}
	delete(m.tasks, id)
	for memoryID, mem := range m.memories {
		if mem.TaskID == id {
			delete(m.memories, memoryID)
		}
	}
	delete(m.leases, id)
	delete(m.indexing, id)
	return true, nil
}

func (m *MemoryStore) DeleteAllTasks(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := int64(len(m.tasks))
	for memoryID, mem := range m.memories {
		if _, taskOwned := m.tasks[mem.TaskID]; taskOwned {
			delete(m.memories, memoryID)
		}
	}
	m.tasks = make(map[string]*types.Task)
	m.leases = make(map[string]memoryLease)
	m.indexing = make(map[string]bool)
	return count, nil
}

// SaveMemory stores a memory.
func (m *MemoryStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cloned := *mem
	cloned.Timestamp = mem.Timestamp.UTC()
	if mem.Embedding != nil {
		cloned.Embedding = make([]float32, len(mem.Embedding))
		copy(cloned.Embedding, mem.Embedding)
	}
	m.memories[mem.ID] = &cloned
	return nil
}

func (m *MemoryStore) ListMemories(_ context.Context, filter ListMemoryFilter) ([]*types.Memory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*types.Memory, 0, len(m.memories))
	for _, mem := range m.memories {
		if filter.TenantID != "" && !memoryTenantMatches(filter.TenantID, mem.TenantID) {
			continue
		}
		if filter.SessionID != "" && mem.SessionID != filter.SessionID {
			continue
		}
		cloned := *mem
		if mem.Embedding != nil {
			cloned.Embedding = append([]float32(nil), mem.Embedding...)
		}
		items = append(items, &cloned)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Timestamp.Equal(items[j].Timestamp) {
			return items[i].ID < items[j].ID
		}
		return items[i].Timestamp.After(items[j].Timestamp)
	})
	limit := resolveLimit(filter.Limit, 50, 500)
	if filter.Offset >= len(items) {
		return []*types.Memory{}, nil
	}
	items = items[filter.Offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (m *MemoryStore) DeleteMemory(_ context.Context, id, tenantID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, exists := m.memories[id]
	if !exists || (tenantID != "" && !memoryTenantMatches(tenantID, mem.TenantID)) {
		return false, nil
	}
	delete(m.memories, id)
	return true, nil
}

func (m *MemoryStore) DeleteAllMemories(_ context.Context, tenantID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, mem := range m.memories {
		if tenantID == "" || memoryTenantMatches(tenantID, mem.TenantID) {
			delete(m.memories, id)
			count++
		}
	}
	return count, nil
}

// QueryMemories searches for memories. If embedding is provided, it uses Cosine Similarity.
// Otherwise, it does keyword-based relevance matching.
func (m *MemoryStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	_, span := tracer.Start(ctx, "store.memory.query_memories")
	defer span.End()
	span.SetAttributes(
		attribute.Bool("agent.query.has_embedding", len(embedding) > 0),
		attribute.Int("agent.query.embedding_dim", len(embedding)),
		attribute.Int("agent.query.limit", limit),
	)

	m.mu.RLock()
	defer m.mu.RUnlock()

	decayRate := 0.0
	if cfg := config.Get(); cfg != nil {
		decayRate = cfg.Store.MemoryDecayRate
	}
	now := time.Now()

	type result struct {
		mem   *types.Memory
		score float32
	}
	var list []result
	mismatchedEmbeddings := 0

	for _, mem := range m.memories {
		if scopedTenant, scoped := tenantScope(ctx); scoped && !memoryTenantMatches(scopedTenant, mem.TenantID) {
			continue
		}
		if scopedSession, scoped := sessionScope(ctx); scoped && mem.SessionID != scopedSession {
			continue
		}
		score, mismatch := memoryRelevanceScore(query, embedding, mem)
		if mismatch {
			mismatchedEmbeddings++
		}
		score = memory.ApplyTimeDecay(score, mem.Timestamp, now, decayRate)
		list = append(list, result{mem: mem, score: score})
	}

	// Sort descending by score
	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})

	if limit > len(list) {
		limit = len(list)
	}

	res := make([]*types.Memory, 0, limit)
	for i := 0; i < limit; i++ {
		// Clone memory to return
		cloned := *list[i].mem
		if list[i].mem.Embedding != nil {
			cloned.Embedding = make([]float32, len(list[i].mem.Embedding))
			copy(cloned.Embedding, list[i].mem.Embedding)
		}
		res = append(res, &cloned)
	}
	span.SetAttributes(
		attribute.Int("agent.store.memory_candidate_count", len(list)),
		attribute.Int("agent.store.memory_count", len(res)),
		attribute.Int("agent.store.embedding_dimension_mismatch_count", mismatchedEmbeddings),
	)
	return res, nil
}

func (m *MemoryStore) CreateSession(_ context.Context, session *types.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]*types.Session)
	}
	if _, exists := m.sessions[session.ID]; exists {
		return fmt.Errorf("session already exists")
	}
	now := time.Now().UTC()
	clone := *session
	if clone.Status == "" {
		clone.Status = types.SessionStatusActive
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	m.sessions[clone.ID] = &clone
	*session = clone
	return nil
}

func (m *MemoryStore) GetSession(_ context.Context, id, tenantID string) (*types.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session := m.sessions[id]
	if session == nil || (tenantID != "" && session.TenantID != tenantID) {
		return nil, ErrSessionNotFound
	}
	clone := *session
	return &clone, nil
}

func (m *MemoryStore) ListSessions(_ context.Context, filter ListSessionFilter) ([]*types.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*types.Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		if filter.TenantID != "" && session.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && session.Status != filter.Status {
			continue
		}
		clone := *session
		items = append(items, &clone)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	limit := resolveLimit(filter.Limit, 50, 500)
	if filter.Offset >= len(items) {
		return []*types.Session{}, nil
	}
	items = items[filter.Offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (m *MemoryStore) UpdateSession(_ context.Context, session *types.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.sessions[session.ID]
	if existing == nil || existing.TenantID != session.TenantID {
		return ErrSessionNotFound
	}
	existing.Title = session.Title
	existing.Status = session.Status
	existing.UpdatedAt = time.Now().UTC()
	*session = *existing
	return nil
}

func (m *MemoryStore) NextSessionTaskSequence(_ context.Context, id, tenantID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil || session.TenantID != tenantID {
		return 0, ErrSessionNotFound
	}
	if session.Status != types.SessionStatusActive {
		return 0, ErrSessionArchived
	}
	session.NextSequence++
	session.UpdatedAt = time.Now().UTC()
	return session.NextSequence, nil
}

func keywordOverlap(query, text string) float32 {
	qWords := strings.Fields(strings.ToLower(query))
	tWords := strings.ToLower(text)
	if len(qWords) == 0 {
		return 0
	}
	var matches float32
	for _, qw := range qWords {
		qw = strings.Trim(qw, ".,!?;:()[]{}'\"-")
		if len(qw) > 2 && strings.Contains(tWords, qw) {
			matches += 1.0
		}
	}
	return matches / float32(len(qWords))
}

// memoryRelevanceScore uses vector similarity only for compatible embeddings.
// Historical local fallback vectors (128 dimensions) can coexist with remote
// provider vectors; keyword scoring keeps those memories retrievable without
// treating an invalid cosine comparison as a real zero score.
func memoryRelevanceScore(query string, queryEmbedding []float32, mem *types.Memory) (float32, bool) {
	if mem != nil && memory.EmbeddingsCompatible(queryEmbedding, mem.Embedding) {
		return memory.CosineSimilarity(queryEmbedding, mem.Embedding), false
	}
	mismatch := mem != nil && len(queryEmbedding) > 0 && len(mem.Embedding) > 0
	if mem == nil {
		return 0, mismatch
	}
	return keywordOverlap(query, mem.Goal+" "+mem.KeyFindings+" "+mem.FinalAnswer), mismatch
}

// TryTransitionTaskStatus atomically attempts to transition a task's status from one of the allowed 'from' statuses to a target status.
// It returns (true, nil) if the transition succeeded, or (false, nil) if the status did not match.
func (m *MemoryStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, exists := m.tasks[id]
	if !exists {
		return false, sql.ErrNoRows
	}

	matched := false
	for _, status := range from {
		if task.Status == status {
			matched = true
			break
		}
	}

	if !matched {
		return false, nil
	}

	task.Status = to
	return true, nil
}

func (m *MemoryStore) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if owner == "" || ttl <= 0 {
		return false, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if lease, ok := m.leases[id]; ok && lease.owner != owner && lease.expiresAt.After(now) {
		return false, nil
	}
	m.leases[id] = memoryLease{owner: owner, expiresAt: now.Add(ttl)}
	return true, nil
}

func (m *MemoryStore) ReleaseTaskLease(ctx context.Context, id, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease, ok := m.leases[id]; ok && lease.owner == owner {
		delete(m.leases, id)
	}
	return nil
}

func (m *MemoryStore) CreateApproval(ctx context.Context, approval *types.DurableApproval) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cloned := types.CloneDurableApproval(approval)
	if err := cloned.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if cloned.CreatedAt.IsZero() {
		cloned.CreatedAt = now
	}
	cloned.UpdatedAt = now
	if cloned.Version == 0 {
		cloned.Version = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.approvals == nil {
		m.approvals = make(map[string]*types.DurableApproval)
	}
	if _, exists := m.approvals[cloned.ID]; exists {
		return fmt.Errorf("approval %q already exists", cloned.ID)
	}
	m.approvals[cloned.ID] = cloned
	return nil
}

func (m *MemoryStore) GetApproval(ctx context.Context, id, tenantID string) (*types.DurableApproval, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	approval, exists := m.approvals[id]
	if !exists || approval.TenantID != tenantID {
		return nil, sql.ErrNoRows
	}
	return types.CloneDurableApproval(approval), nil
}

func (m *MemoryStore) ListTaskApprovals(ctx context.Context, taskID, tenantID string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*types.DurableApproval, 0)
	for _, approval := range m.approvals {
		if approval.TaskID == taskID && approval.TenantID == tenantID && (status == "" || approval.Status == status) {
			result = append(result, types.CloneDurableApproval(approval))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (m *MemoryStore) TransitionApproval(ctx context.Context, id, tenantID string, expectedVersion int64, from, to types.DurableApprovalStatus, resolutionPayload []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !types.CanTransitionApproval(from, to) {
		return false, fmt.Errorf("invalid approval transition %s -> %s", from, to)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	approval, exists := m.approvals[id]
	if !exists || approval.TenantID != tenantID || approval.Version != expectedVersion || approval.Status != from {
		return false, nil
	}
	now := time.Now().UTC()
	approval.Status = to
	approval.Version++
	approval.UpdatedAt = now
	approval.ResolutionPayload = append([]byte(nil), resolutionPayload...)
	if to != types.ApprovalPending {
		approval.ResolvedAt = now
	}
	if to == types.ApprovalConsumed || to == types.ApprovalRejected || to == types.ApprovalExpired {
		approval.Owner = ""
		approval.LeaseExpiresAt = time.Time{}
	}
	return true, nil
}

func (m *MemoryStore) AcquireApprovalLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	approval, exists := m.approvals[id]
	if !exists {
		return false, sql.ErrNoRows
	}
	now := time.Now().UTC()
	if approval.Owner != "" && approval.Owner != owner && approval.LeaseExpiresAt.After(now) {
		return false, nil
	}
	approval.Owner = owner
	approval.LeaseExpiresAt = now.Add(ttl)
	return true, nil
}

func (m *MemoryStore) ReleaseApprovalLease(ctx context.Context, id, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if approval := m.approvals[id]; approval != nil && approval.Owner == owner {
		approval.Owner = ""
		approval.LeaseExpiresAt = time.Time{}
	}
	return nil
}

func (m *MemoryStore) DeleteTerminalApprovalsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var deleted int64
	for id, approval := range m.approvals {
		terminal := approval.Status == types.ApprovalConsumed || approval.Status == types.ApprovalExpired
		if terminal && !approval.ResolvedAt.IsZero() && approval.ResolvedAt.Before(cutoff) {
			delete(m.approvals, id)
			deleted++
		}
	}
	return deleted, nil
}
