package store

import "sync"

// memoryIndexGate deduplicates indexing and closes admission before waiting.
// The mutex makes Wait safe even when saves race store shutdown.
type memoryIndexGate struct {
	mu       sync.Mutex
	inflight map[string]bool
	closed   bool
	wg       sync.WaitGroup
}

func (g *memoryIndexGate) tryStart(memoryID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if memoryID == "" || g.closed || g.inflight[memoryID] {
		return false
	}
	if g.inflight == nil {
		g.inflight = make(map[string]bool)
	}
	g.inflight[memoryID] = true
	g.wg.Add(1)
	return true
}

func (g *memoryIndexGate) done(memoryID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[memoryID] {
		delete(g.inflight, memoryID)
		g.wg.Done()
	}
}

func (g *memoryIndexGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.wg.Wait()
}
