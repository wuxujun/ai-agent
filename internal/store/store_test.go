package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStores(t *testing.T) {
	// Setup a temporary directory for SQLite file
	tmpDir, err := os.MkdirTemp("", "store_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sqlitePath := filepath.Join(tmpDir, "test.db")
	sqliteStore, err := store.NewSQLiteStore(sqlitePath)
	if err != nil {
		t.Fatalf("failed to create SQLiteStore: %v", err)
	}

	memoryStore := store.NewMemoryStore()

	stores := map[string]store.Store{
		"SQLiteStore": sqliteStore,
		"MemoryStore": memoryStore,
	}

	// Optional: PostgreSQL test if TEST_POSTGRES_DSN is provided
	// e.g. TEST_POSTGRES_DSN="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	pgDsn := os.Getenv("TEST_POSTGRES_DSN")
	if pgDsn != "" {
		pgStore, err := store.NewPostgresStore(pgDsn)
		if err != nil {
			t.Fatalf("failed to create PostgresStore: %v", err)
		}
		stores["PostgresStore"] = pgStore
	} else {
		t.Log("Skipping PostgresStore test: TEST_POSTGRES_DSN environment variable not set")
	}

	// Optional: Redis test if TEST_REDIS_URL is provided
	// e.g. TEST_REDIS_URL="redis://localhost:6379/0"
	redisUrl := os.Getenv("TEST_REDIS_URL")
	if redisUrl != "" {
		redisStore, err := store.NewRedisStoreFromURL(redisUrl)
		if err != nil {
			t.Fatalf("failed to create RedisStore: %v", err)
		}
		stores["RedisStore"] = redisStore
	} else {
		t.Log("Skipping RedisStore test: TEST_REDIS_URL environment variable not set")
	}

	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			defer s.Close()
			ctx := context.Background()

			// 1. Verify task not found returns sql.ErrNoRows
			_, err := s.GetTask(ctx, "non-existent")
			if err != sql.ErrNoRows {
				t.Errorf("expected sql.ErrNoRows, got %v", err)
			}

			// 2. Save a task
			task := &types.Task{
				ID:         "task-123",
				Goal:       "Build a cool agent",
				Status:     types.StatusCreated,
				MaxSteps:   10,
				StepCount:  1,
				Workspace:  "/tmp/workspace",
				Hypothesis: "Initial hypothesis",
				Unresolved: []string{"subtask-a", "subtask-b"},
				ToolBudget: 5,
				Trace: []types.StepTrace{
					{
						Step:        1,
						Goal:        "Find file",
						Action:      "find_files",
						Query:       "*.go",
						Observation: "found main.go",
						Evidence: []types.Evidence{
							{
								Path:  "main.go",
								Lines: []string{"package main"},
								Query: "*.go",
							},
						},
					},
				},
				FinalAnswer: "Done!",
			}

			err = s.SaveFullTask(ctx, task)
			if err != nil {
				t.Fatalf("failed to save task: %v", err)
			}

			// 3. Retrieve and verify the task
			retrieved, err := s.GetTask(ctx, "task-123")
			if err != nil {
				t.Fatalf("failed to get task: %v", err)
			}

			if retrieved.ID != task.ID {
				t.Errorf("expected ID %q, got %q", task.ID, retrieved.ID)
			}
			if retrieved.Goal != task.Goal {
				t.Errorf("expected Goal %q, got %q", task.Goal, retrieved.Goal)
			}
			if retrieved.Status != task.Status {
				t.Errorf("expected Status %q, got %q", task.Status, retrieved.Status)
			}
			if retrieved.MaxSteps != task.MaxSteps {
				t.Errorf("expected MaxSteps %d, got %d", task.MaxSteps, retrieved.MaxSteps)
			}
			if retrieved.StepCount != task.StepCount {
				t.Errorf("expected StepCount %d, got %d", task.StepCount, retrieved.StepCount)
			}
			if retrieved.Workspace != task.Workspace {
				t.Errorf("expected Workspace %q, got %q", task.Workspace, retrieved.Workspace)
			}
			if retrieved.Hypothesis != task.Hypothesis {
				t.Errorf("expected Hypothesis %q, got %q", task.Hypothesis, retrieved.Hypothesis)
			}
			if len(retrieved.Unresolved) != len(task.Unresolved) {
				t.Errorf("expected Unresolved len %d, got %d", len(task.Unresolved), len(retrieved.Unresolved))
			} else {
				for i, v := range task.Unresolved {
					if retrieved.Unresolved[i] != v {
						t.Errorf("expected Unresolved[%d] = %q, got %q", i, v, retrieved.Unresolved[i])
					}
				}
			}
			if retrieved.ToolBudget != task.ToolBudget {
				t.Errorf("expected ToolBudget %d, got %d", task.ToolBudget, retrieved.ToolBudget)
			}
			if retrieved.FinalAnswer != task.FinalAnswer {
				t.Errorf("expected FinalAnswer %q, got %q", task.FinalAnswer, retrieved.FinalAnswer)
			}

			// 4. Verify step traces
			if len(retrieved.Trace) != len(task.Trace) {
				t.Fatalf("expected Trace len %d, got %d", len(task.Trace), len(retrieved.Trace))
			}

			tr1 := task.Trace[0]
			tr2 := retrieved.Trace[0]
			if tr1.Step != tr2.Step {
				t.Errorf("expected trace Step %d, got %d", tr1.Step, tr2.Step)
			}
			if tr1.Goal != tr2.Goal {
				t.Errorf("expected trace Goal %q, got %q", tr1.Goal, tr2.Goal)
			}
			if tr1.Action != tr2.Action {
				t.Errorf("expected trace Action %q, got %q", tr1.Action, tr2.Action)
			}
			if tr1.Query != tr2.Query {
				t.Errorf("expected trace Query %q, got %q", tr1.Query, tr2.Query)
			}
			if tr1.Observation != tr2.Observation {
				t.Errorf("expected trace Observation %q, got %q", tr1.Observation, tr2.Observation)
			}

			if len(tr1.Evidence) != len(tr2.Evidence) {
				t.Fatalf("expected evidence len %d, got %d", len(tr1.Evidence), len(tr2.Evidence))
			}
			ev1 := tr1.Evidence[0]
			ev2 := tr2.Evidence[0]
			if ev1.Path != ev2.Path {
				t.Errorf("expected evidence Path %q, got %q", ev1.Path, ev2.Path)
			}
			if ev1.Query != ev2.Query {
				t.Errorf("expected evidence Query %q, got %q", ev1.Query, ev2.Query)
			}
			if len(ev1.Lines) != len(ev2.Lines) || ev1.Lines[0] != ev2.Lines[0] {
				t.Errorf("expected evidence Lines %v, got %v", ev1.Lines, ev2.Lines)
			}
		})
	}
}
