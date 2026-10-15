package multiagent

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestPersistTaskDetachedPreservesLeaseAndRequestScope(t *testing.T) {
	// A guarded callback models the store lease scope. Importing store here would
	// introduce the existing store -> memory -> multiagent dependency cycle.
	type ownerKey struct{}
	type requestKey struct{}
	lost := errors.New("fixture ownership lost")
	task := &types.Task{ID: "checkpoint", TenantID: "tenant", Goal: "original", Status: types.StatusRunning}
	owner := "owner"
	persisted := "original"
	base := context.WithValue(t.Context(), requestKey{}, "retained")
	base = context.WithValue(base, ownerKey{}, owner)
	canceled, cancel := context.WithCancel(base)
	cancel()
	c := &Coordinator{PersistTask: func(ctx context.Context, task *types.Task) error {
		if ctx.Err() != nil || ctx.Value(requestKey{}) != "retained" {
			t.Fatal("detached request scope lost")
		}
		if ctx.Value(ownerKey{}) != owner {
			return lost
		}
		persisted = task.Goal
		return nil
	}}
	task.Goal = "updated"
	if err := c.persistTaskDetached(canceled, task); err != nil {
		t.Fatal(err)
	}
	owner = "replacement"
	task.Goal = "stale-write"
	if err := c.persistTaskDetached(canceled, task); !errors.Is(err, lost) {
		t.Fatal("stale scope bypassed callback guard", err)
	}
	if persisted != "updated" {
		t.Fatal("stale checkpoint persisted")
	}
}
