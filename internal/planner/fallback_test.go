package planner

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

type failingPlanner struct {
	err error
}

func (p failingPlanner) PlanNext(context.Context, *types.Task, func(string)) (*PlanDecision, error) {
	return nil, p.err
}

func TestFallbackPlannerReturnsPrimaryErrorWithoutSecondary(t *testing.T) {
	want := errors.New("planner provider unavailable")
	planner := &FallbackPlanner{Primary: failingPlanner{err: want}}

	decision, err := planner.PlanNext(context.Background(), &types.Task{ID: "provider-failure"}, nil)
	if decision != nil {
		t.Fatalf("expected no decision, got %#v", decision)
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected primary error %v, got %v", want, err)
	}
}
