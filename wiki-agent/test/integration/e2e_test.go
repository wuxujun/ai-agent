//go:build integration

// Package integration exercises the full Orchestrator → Planner → Wiki Agent
// → Validation Gateway → PostgreSQL/pgvector loop.
package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/planner"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/storage/postgres"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/testdb"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/validation"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/wiki"
	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// scriptedExecutor returns low-confidence findings on the first iteration and
// confident ones afterwards, forcing one re-plan.
type scriptedExecutor struct{ calls int }

func (e *scriptedExecutor) Execute(_ context.Context, task models.Task, step models.Step) ([]models.Finding, error) {
	e.calls++
	confidence := 0.95
	if strings.Contains(step.ID, "-i0-") { // planner step IDs are "<task>-i<iteration>-s<n>"
		confidence = 0.4
	}
	return []models.Finding{{
		StepID:     step.ID,
		Source:     "scripted-executor",
		URI:        "finding://" + task.ID + "/" + step.ID,
		Summary:    "pagination verified",
		Content:    "Confirmed GET /users supports cursor pagination via the next_cursor field.",
		Confidence: confidence,
		ObservedAt: time.Now().UTC(),
	}}, nil
}

func TestEndToEndWorkflow(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)

	gateway := validation.NewGateway(validation.Config{MinConfidence: 0.7, MaxAge: 24 * time.Hour})
	agent := wiki.NewAgent(postgres.NewRepository(pool), wiki.HashEmbedder{Dims: models.EmbeddingDimensions},
		wiki.ParagraphChunker{MaxChars: 500}, gateway, nil)

	seed := []struct {
		doc        models.Document
		confidence float64
		accepted   bool
	}{
		{models.Document{Source: "openapi", URI: "spec://users", Kind: models.KindAPISpec, Title: "Users API",
			Content: "GET /users lists users with cursor pagination. The response includes next_cursor."}, 0.95, true},
		{models.Document{Source: "wiki", URI: "doc://style", Kind: models.KindDoc, Title: "Go style guide",
			Content: "Handlers must return JSON errors with an error field."}, 0.9, true},
		{models.Document{Source: "forum", URI: "doc://rumor", Kind: models.KindDoc, Title: "Unverified rumor",
			Content: "GET /users pagination uses page numbers."}, 0.3, false},
	}
	for _, s := range seed {
		res, err := agent.Ingest(ctx, wiki.IngestRequest{Document: s.doc, Confidence: s.confidence})
		if s.accepted && err != nil {
			t.Fatalf("ingest %s: %v", s.doc.URI, err)
		}
		if !s.accepted && res.Validation.Accepted() {
			t.Fatalf("%s should have been rejected", s.doc.URI)
		}
	}

	exec := &scriptedExecutor{}
	orch := orchestrator.New(
		planner.New(agent, nil, models.SearchOptions{Limit: 2}),
		exec, agent, orchestrator.Config{MaxIterations: 3}, nil,
	)
	wf, err := orch.Run(ctx, models.Task{ID: "task-1", Goal: "Add cursor pagination to GET /users"})
	if err != nil {
		t.Fatalf("run: %v (workflow: %+v)", err, wf)
	}
	if wf.State != models.StateCompleted || wf.Iteration != 1 {
		t.Fatalf("state=%s iteration=%d", wf.State, wf.Iteration)
	}
	for _, k := range wf.Plan.Knowledge {
		if k.Document.URI == "doc://rumor" {
			t.Fatal("plan was grounded in rejected knowledge")
		}
	}
	if wf.Plan.Knowledge[0].Document.URI != "spec://users" {
		t.Fatalf("most similar knowledge = %s, want spec://users", wf.Plan.Knowledge[0].Document.URI)
	}

	// Verified findings are persisted and become retrievable knowledge.
	var findings int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM documents d JOIN validations v ON v.document_id = d.id
		WHERE d.kind = 'finding' AND v.status = 'accepted'`).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if findings == 0 {
		t.Fatal("no verified findings persisted")
	}
	got, err := agent.Retrieve(ctx, "cursor pagination next_cursor", models.SearchOptions{Limit: 5, Kinds: []models.DocumentKind{models.KindFinding}})
	if err != nil || len(got) == 0 {
		t.Fatalf("findings not retrievable: %v %+v", err, got)
	}
}
