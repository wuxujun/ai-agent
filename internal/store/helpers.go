package store

import (
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func normalizeTaskTimestamps(task *types.Task) {
	if task == nil {
		return
	}
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
}

// defaultMemoryCandidateLimit is the cap applied to candidate memory rows when
// store.memory_candidate_limit is 0 or negative in config. Mirrors the original
// hardcoded SQLite limit so existing deployments are unchanged.
const defaultMemoryCandidateLimit = 200

var memoryCandidateLimitWarnings sync.Map

// resolveMemoryCandidateLimit returns the effective candidate cap for
// QueryMemories backends. Reads the live config at call time (do not cache)
// so a hot-reload of store.memory_candidate_limit takes effect on the next
// RAG prefetch without a restart.
func resolveMemoryCandidateLimit() int {
	if cfg := config.Get(); cfg != nil && cfg.Store.MemoryCandidateLimit > 0 {
		return cfg.Store.MemoryCandidateLimit
	}
	return defaultMemoryCandidateLimit
}

func warnMemoryCandidateLimitReached(backend string, candidateLimit int) {
	key := fmt.Sprintf("%s:%d", backend, candidateLimit)
	if _, loaded := memoryCandidateLimitWarnings.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Warn("memory candidate scan hit store.memory_candidate_limit; older rows excluded from ranking",
		"candidate_limit", candidateLimit,
		"backend", backend,
	)
}

// memoriesForPersistence returns a copy of mems with Embedding cleared on each
// element. The embedding can be ~1.5 KB per memory (128–1536 float32 values)
// and is never read after retrieval — formatMemories in the planner agents
// only uses Goal / FinalAnswer / KeyFindings. Stripping it keeps the on-disk
// JSON small and consistent across sqlite / postgres / redis backends.
//
// Dedupe of incoming RAG memories happens upstream in
// memory.DeduplicateMemories (engine.go), so the persisted shape doesn't need
// embeddings to support that path either.
func memoriesForPersistence(mems []types.Memory) []types.Memory {
	if len(mems) == 0 {
		return nil
	}
	out := make([]types.Memory, len(mems))
	for i, m := range mems {
		out[i] = m
		out[i].Embedding = nil
	}
	return out
}
