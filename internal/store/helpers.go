package store

import "github.com/wuxujun/ai-agent/internal/types"

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
