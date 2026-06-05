package store

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
)

// MemoryStore implements Store in-memory for testing or ephemeral executions.
type MemoryStore struct {
	mu       sync.RWMutex
	tasks    map[string]*types.Task
	memories map[string]*types.Memory
}

// NewMemoryStore initializes a new MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:    make(map[string]*types.Task),
		memories: make(map[string]*types.Memory),
	}
}


// SaveFullTask saves or updates a task and its traces in memory.
func (m *MemoryStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	m.mu.Lock()

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
	m.mu.Unlock()

	if task.Status == types.StatusCompleted {
		// Asynchronously index completed task as a long-term memory for cross-task RAG.
		// Since generating embeddings can take time (e.g. hitting remote APIs),
		// we run this outside of the write lock to prevent blocking memory storage.
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			mem, err := memory.CreateMemoryFromTask(bgCtx, &cloned)
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

// ListTasks returns tasks matching f. MemoryStore applies status filter and pagination in-process.
func (m *MemoryStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tasks := make([]*types.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		cloned := *t
		if t.Unresolved != nil {
			cloned.Unresolved = make([]string, len(t.Unresolved))
			copy(cloned.Unresolved, t.Unresolved)
		}
		if t.Trace != nil {
			cloned.Trace = make([]types.StepTrace, len(t.Trace))
			copy(cloned.Trace, t.Trace)
		}
		tasks = append(tasks, &cloned)
	}

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

// SaveMemory stores a memory.
func (m *MemoryStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cloned := *mem
	if mem.Embedding != nil {
		cloned.Embedding = make([]float32, len(mem.Embedding))
		copy(cloned.Embedding, mem.Embedding)
	}
	m.memories[mem.ID] = &cloned
	return nil
}

// QueryMemories searches for memories. If embedding is provided, it uses Cosine Similarity.
// Otherwise, it does keyword-based relevance matching.
func (m *MemoryStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	type result struct {
		mem   *types.Memory
		score float32
	}
	var list []result

	for _, mem := range m.memories {
		var score float32
		if len(embedding) > 0 && len(mem.Embedding) > 0 {
			// Cosine Similarity
			score = cosineSimilarity(embedding, mem.Embedding)
		} else {
			// Keyword match score as fallback
			score = keywordOverlap(query, mem.Goal+" "+mem.KeyFindings+" "+mem.FinalAnswer)
		}
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
	return res, nil
}

func cosineSimilarity(v1, v2 []float32) float32 {
	if len(v1) != len(v2) || len(v1) == 0 {
		return 0
	}
	var dotProduct, norm1, norm2 float64
	for i := range v1 {
		dotProduct += float64(v1[i] * v2[i])
		norm1 += float64(v1[i] * v1[i])
		norm2 += float64(v2[i] * v2[i])
	}
	if norm1 == 0 || norm2 == 0 {
		return 0
	}
	return float32(dotProduct / (math.Sqrt(norm1) * math.Sqrt(norm2)))
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

