package api

import (
	"context"
	"sync"
)

type waiterState int

const (
	waiterStatePending waiterState = iota
	waiterStateGranted
	waiterStateCancelled
)

type semWaiter struct {
	ch    chan struct{}
	state waiterState
}

type resizableSemaphore struct {
	mu      sync.Mutex
	limit   int
	current int
	waiters []*semWaiter
}

func newResizableSemaphore(limit int) *resizableSemaphore {
	if limit <= 0 {
		limit = 10
	}
	return &resizableSemaphore{limit: limit}
}

func (s *resizableSemaphore) Acquire(ctx context.Context) bool {
	s.mu.Lock()
	if s.current < s.limit {
		s.current++
		s.mu.Unlock()
		return true
	}

	w := &semWaiter{ch: make(chan struct{}), state: waiterStatePending}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	select {
	case <-w.ch:
		return true
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		switch w.state {
		case waiterStateGranted:
			// A grant raced with cancellation. Return the slot so capacity does not leak.
			s.current--
			s.wakeWaitersLocked()
		case waiterStatePending:
			w.state = waiterStateCancelled
			s.removeWaiterLocked(w)
		}
		return false
	}
}

func (s *resizableSemaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current > 0 {
		s.current--
	}
	s.wakeWaitersLocked()
}

func (s *resizableSemaphore) Resize(limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	s.limit = limit
	s.wakeWaitersLocked()
}

func (s *resizableSemaphore) removeWaiterLocked(target *semWaiter) {
	for i, w := range s.waiters {
		if w == target {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

func (s *resizableSemaphore) wakeWaitersLocked() {
	for len(s.waiters) > 0 && s.current < s.limit {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		if w.state != waiterStatePending {
			continue
		}
		w.state = waiterStateGranted
		s.current++
		close(w.ch)
	}
}
