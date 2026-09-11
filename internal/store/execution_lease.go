package store

import (
	"context"
	"sync"
	"time"
)

// ExecutionLease keeps ownership alive even while approval pauses execution or
// cancellation flushes its final state. Its lifetime ends before lease release.
type ExecutionLease struct {
	mu         sync.Mutex
	validUntil time.Time
	lost       bool
	onLoss     func()
	stop       context.CancelFunc
	done       chan struct{}
	expiry     *time.Timer
}

// NewExecutionLease creates a lease monitor. Set onLoss before starting; it must
// be concurrency safe and idempotent, typically a context cancellation function.
func NewExecutionLease(onLoss func()) *ExecutionLease {
	return &ExecutionLease{onLoss: onLoss}
}

// resetExpiryLocked keeps expiry independent of a stalled renewal request.
func (l *ExecutionLease) resetExpiryLocked() {
	if l.expiry != nil {
		l.expiry.Stop()
	}
	l.expiry = time.AfterFunc(time.Until(l.validUntil), func() { _ = l.Err() })
}

func (l *ExecutionLease) Err() error {
	l.mu.Lock()
	if !l.validUntil.IsZero() && !time.Now().Before(l.validUntil) {
		l.lost = true
	}
	lost := l.lost
	l.mu.Unlock()
	if lost {
		l.onLoss()
		return ErrTaskLeaseLost
	}
	return nil
}

func (l *ExecutionLease) Lose() {
	l.mu.Lock()
	l.lost = true
	l.mu.Unlock()
	l.onLoss()
}

func (l *ExecutionLease) Start(base context.Context, st TaskLeaseStore, id, owner string, ttl time.Duration, acquiredAt time.Time) {
	l.mu.Lock()
	l.validUntil = acquiredAt.Add(ttl)
	l.resetExpiryLocked()
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.WithoutCancel(base))
	l.stop, l.done = cancel, make(chan struct{})
	go func() {
		defer close(l.done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if l.Err() != nil {
					return
				}
				started := time.Now()
				renewCtx, renewCancel := context.WithTimeout(ctx, min(3*time.Second, ttl/3))
				ok, err := st.RenewTaskLease(renewCtx, id, owner, ttl)
				renewCancel()
				if ctx.Err() != nil {
					return
				}
				l.mu.Lock()
				// A late response must never revive an execution that outlived
				// its last confirmed ownership window.
				if !ok || err != nil || !time.Now().Before(l.validUntil) {
					l.lost = true
				}
				if !l.lost {
					l.validUntil = started.Add(ttl)
					l.resetExpiryLocked()
				}
				lost := l.lost
				l.mu.Unlock()
				if lost {
					l.onLoss()
					return
				}
			}
		}
	}()
}

func (l *ExecutionLease) Finish() {
	l.stop()
	<-l.done
	l.mu.Lock()
	l.expiry.Stop()
	l.mu.Unlock()
}
