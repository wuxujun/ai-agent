package store

import "sync"

// memoryIndexGate prevents repeated SaveFullTask calls in one process from
// starting duplicate asynchronous embedding work for the same memory ID.
// Database uniqueness remains the cross-process final consistency guard.
type memoryIndexGate struct {
	inflight sync.Map
}

func (g *memoryIndexGate) tryStart(memoryID string) bool {
	if memoryID == "" {
		return false
	}
	_, loaded := g.inflight.LoadOrStore(memoryID, struct{}{})
	return !loaded
}

func (g *memoryIndexGate) done(memoryID string) {
	g.inflight.Delete(memoryID)
}
