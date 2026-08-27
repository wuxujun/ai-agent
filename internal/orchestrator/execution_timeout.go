package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"
)

type executionTimeoutKey struct{}

var (
	ErrClientDisconnected = errors.New("streaming client disconnected")
	ErrTaskCanceledViaAPI = errors.New("task canceled via API")
)

type pausableTimeoutContext struct {
	parent context.Context
	done   chan struct{}

	mu        sync.Mutex
	timer     *time.Timer
	remaining time.Duration
	deadline  time.Time
	paused    bool
	err       error
	cause     error
	once      sync.Once
}

// WithPausableTimeout is equivalent to context.WithTimeout while execution is
// active, but lets the approval flow temporarily freeze the remaining budget.
func WithPausableTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	c := &pausableTimeoutContext{parent: parent, done: make(chan struct{}), remaining: timeout}
	c.resumeLocked()
	if parent.Done() != nil {
		go func() {
			select {
			case <-parent.Done():
				c.cancel(parent.Err(), context.Cause(parent))
			case <-c.done:
			}
		}()
	}
	ctx := context.WithValue(c, executionTimeoutKey{}, c)
	return ctx, func() { c.cancel(context.Canceled, context.Canceled) }
}

func (c *pausableTimeoutContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused || c.err != nil {
		return time.Time{}, false
	}
	return c.deadline, true
}

func (c *pausableTimeoutContext) Done() <-chan struct{} { return c.done }

func (c *pausableTimeoutContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *pausableTimeoutContext) Value(key any) any { return c.parent.Value(key) }

func (c *pausableTimeoutContext) cancel(err, cause error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.cause = cause
		if c.timer != nil {
			c.timer.Stop()
		}
		close(c.done)
		c.mu.Unlock()
	})
}

func (c *pausableTimeoutContext) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused || c.err != nil {
		return
	}
	if c.timer != nil && c.timer.Stop() {
		c.remaining = time.Until(c.deadline)
		if c.remaining < 0 {
			c.remaining = 0
		}
	}
	c.paused = true
}

func (c *pausableTimeoutContext) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused || c.err != nil {
		return
	}
	c.resumeLocked()
}

func (c *pausableTimeoutContext) resumeLocked() {
	c.paused = false
	c.deadline = time.Now().Add(c.remaining)
	c.timer = time.AfterFunc(c.remaining, func() { c.cancel(context.DeadlineExceeded, context.DeadlineExceeded) })
}

// CancelExecution cancels a pausable execution context while retaining a
// stable cause for task-result classification. It returns false for contexts
// not created by WithPausableTimeout.
func CancelExecution(ctx context.Context, cause error) bool {
	c, _ := ctx.Value(executionTimeoutKey{}).(*pausableTimeoutContext)
	if c == nil {
		return false
	}
	if cause == nil {
		cause = context.Canceled
	}
	c.cancel(context.Canceled, cause)
	return true
}

func executionCancellationCause(ctx context.Context) error {
	c, _ := ctx.Value(executionTimeoutKey{}).(*pausableTimeoutContext)
	if c == nil {
		return context.Cause(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// PauseExecutionTimeout freezes a timeout created by WithPausableTimeout and
// returns an idempotent resume function. Other contexts are unchanged.
func PauseExecutionTimeout(ctx context.Context) func() {
	c, _ := ctx.Value(executionTimeoutKey{}).(*pausableTimeoutContext)
	if c == nil {
		return func() {}
	}
	c.pause()
	var once sync.Once
	return func() { once.Do(c.resume) }
}
