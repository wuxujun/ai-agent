// Package orchestrator coordinates the Planner, Executor and Wiki Agent,
// drives the workflow state machine and routes inter-agent messages.
package orchestrator

import (
	"context"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// Planner produces a plan grounded in verified knowledge.
type Planner interface {
	Plan(ctx context.Context, req models.PlanRequest) (models.Plan, error)
}

// Executor runs a single plan step and reports findings for verification.
// v1 ships the contract only; implementations live outside this module.
type Executor interface {
	Execute(ctx context.Context, task models.Task, step models.Step) ([]models.Finding, error)
}

// KnowledgeBase accepts executor findings and verifies them through the
// Validation Gateway (implemented by wiki.Agent).
type KnowledgeBase interface {
	SubmitFindings(ctx context.Context, findings []models.Finding) ([]models.ValidationResult, error)
}
