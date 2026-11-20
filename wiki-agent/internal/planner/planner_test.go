package planner

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

type fakeRetriever struct{ chunks []models.RetrievedChunk }

func (f fakeRetriever) Retrieve(context.Context, string, models.SearchOptions) ([]models.RetrievedChunk, error) {
	return f.chunks, nil
}

type fixedGenerator []models.Step

func (g fixedGenerator) Generate(context.Context, models.PlanRequest, []models.RetrievedChunk) ([]models.Step, error) {
	return g, nil
}

var knowledge = []models.RetrievedChunk{
	{Chunk: models.Chunk{ID: "c1"}, Document: models.Document{URI: "doc://a", Title: "A", Kind: models.KindDoc}},
	{Chunk: models.Chunk{ID: "c2"}, Document: models.Document{URI: "doc://a", Title: "A", Kind: models.KindDoc}},
	{Chunk: models.Chunk{ID: "c3"}, Document: models.Document{URI: "spec://b", Kind: models.KindAPISpec}},
}

func TestPlanTemplateGenerator(t *testing.T) {
	p := New(fakeRetriever{knowledge}, nil, models.SearchOptions{})
	plan, err := p.Plan(context.Background(), models.PlanRequest{Task: models.Task{ID: "t1", Goal: "g"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || len(plan.Steps[0].References) != 2 || plan.Steps[1].References[0] != "c3" {
		t.Fatalf("unexpected steps: %+v", plan.Steps)
	}
	if plan.Steps[0].ID != "t1-i0-s1" || len(plan.Knowledge) != 3 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanRejectsUngroundedPlans(t *testing.T) {
	req := models.PlanRequest{Task: models.Task{ID: "t1", Goal: "g"}}
	tests := []struct {
		name string
		p    *Planner
		want error
	}{
		{"no knowledge", New(fakeRetriever{}, nil, models.SearchOptions{}), ErrNoVerifiedKnowledge},
		{"empty plan", New(fakeRetriever{knowledge}, fixedGenerator{}, models.SearchOptions{}), ErrEmptyPlan},
		{"no references", New(fakeRetriever{knowledge}, fixedGenerator{{Action: "x"}}, models.SearchOptions{}), ErrUnverifiedReference},
		{"unknown reference", New(fakeRetriever{knowledge}, fixedGenerator{{Action: "x", References: []string{"c1", "zzz"}}}, models.SearchOptions{}), ErrUnverifiedReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.p.Plan(context.Background(), req); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
