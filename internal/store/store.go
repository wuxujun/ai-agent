package store

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/types"
)

// Store defines the interface for task and trace storage.
type Store interface {
	SaveFullTask(ctx context.Context, task *types.Task) error
	GetTask(ctx context.Context, id string) (*types.Task, error)
	Close() error
}
