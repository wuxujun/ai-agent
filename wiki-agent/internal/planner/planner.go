// Package planner generates execution plans grounded strictly in knowledge
// that the Wiki Agent has verified.
package planner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

var (
	// ErrNoVerifiedKnowledge is returned when retrieval yields nothing to ground a plan in.
	ErrNoVerifiedKnowledge = errors.New("planner: no verified knowledge available for task")
	// ErrUnverifiedReference is returned when a generated step cites knowledge
	// that was not part of the verified retrieval set (or cites nothing).
	ErrUnverifiedReference = errors.New("planner: step references unverified knowledge")
	// ErrEmptyPlan is returned when the generator produces no steps.
	ErrEmptyPlan = errors.New("planner: generator produced no steps")
)

// KnowledgeRetriever returns verified knowledge for a query (implemented by wiki.Agent).
type KnowledgeRetriever interface {
	Retrieve(ctx context.Context, query string, opts models.SearchOptions) ([]models.RetrievedChunk, error)
}

// Generator turns a task and verified knowledge into steps. Swap in an
// LLM-backed implementation without changing the Planner's guarantees.
type Generator interface {
	Generate(ctx context.Context, req models.PlanRequest, knowledge []models.RetrievedChunk) ([]models.Step, error)
}

// Planner enforces that every step is grounded in verified knowledge.
type Planner struct {
	knowledge KnowledgeRetriever
	generator Generator
	search    models.SearchOptions
	now       func() time.Time
}

// New creates a Planner. A nil generator uses TemplateGenerator.
func New(knowledge KnowledgeRetriever, generator Generator, search models.SearchOptions) *Planner {
	if generator == nil {
		generator = TemplateGenerator{}
	}
	return &Planner{knowledge: knowledge, generator: generator, search: search, now: time.Now}
}

// Plan implements orchestrator.Planner.
func (p *Planner) Plan(ctx context.Context, req models.PlanRequest) (models.Plan, error) {
	query := strings.TrimSpace(req.Task.Goal + "\n" + req.Task.Description)
	knowledge, err := p.knowledge.Retrieve(ctx, query, p.search)
	if err != nil {
		return models.Plan{}, fmt.Errorf("planner: retrieve: %w", err)
	}
	if len(knowledge) == 0 {
		return models.Plan{}, ErrNoVerifiedKnowledge
	}
	steps, err := p.generator.Generate(ctx, req, knowledge)
	if err != nil {
		return models.Plan{}, fmt.Errorf("planner: generate: %w", err)
	}
	if len(steps) == 0 {
		return models.Plan{}, ErrEmptyPlan
	}

	verified := make(map[string]bool, len(knowledge))
	for _, k := range knowledge {
		verified[k.Chunk.ID] = true
	}
	for i := range steps {
		if steps[i].ID == "" {
			steps[i].ID = fmt.Sprintf("%s-i%d-s%d", req.Task.ID, req.Iteration, i+1)
		}
		if len(steps[i].References) == 0 {
			return models.Plan{}, fmt.Errorf("%w: step %s cites no knowledge", ErrUnverifiedReference, steps[i].ID)
		}
		for _, ref := range steps[i].References {
			if !verified[ref] {
				return models.Plan{}, fmt.Errorf("%w: step %s cites %q", ErrUnverifiedReference, steps[i].ID, ref)
			}
		}
	}
	return models.Plan{
		TaskID:    req.Task.ID,
		Iteration: req.Iteration,
		Steps:     steps,
		Knowledge: knowledge,
		CreatedAt: p.now().UTC(),
	}, nil
}

// TemplateGenerator is a deterministic generator producing one step per
// source document. It is the v1 default until an LLM generator is plugged in.
type TemplateGenerator struct{}

// Generate implements Generator.
func (TemplateGenerator) Generate(_ context.Context, req models.PlanRequest, knowledge []models.RetrievedChunk) ([]models.Step, error) {
	var steps []models.Step
	index := map[string]int{}
	for _, k := range knowledge {
		key := k.Document.URI
		i, ok := index[key]
		if !ok {
			title := k.Document.Title
			if title == "" {
				title = k.Document.URI
			}
			rationale := fmt.Sprintf("Grounded in verified %s %q (similarity %.2f)", k.Document.Kind, title, k.Similarity)
			if len(req.Feedback) > 0 {
				rationale += fmt.Sprintf("; addresses %d verification issue(s) from iteration %d", len(req.Feedback), req.Iteration-1)
			}
			steps = append(steps, models.Step{
				Action:    fmt.Sprintf("Apply %s to: %s", title, req.Task.Goal),
				Rationale: rationale,
			})
			i = len(steps) - 1
			index[key] = i
		}
		steps[i].References = append(steps[i].References, k.Chunk.ID)
	}
	return steps, nil
}
