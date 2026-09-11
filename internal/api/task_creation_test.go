package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Hold both callers at the persistence boundary so the result does not
// depend on the scheduler choosing a rare check-then-save interleaving.
type concurrentCreationStore struct {
	*store.MemoryStore
	checked      atomic.Int32
	creating     atomic.Int32
	checksDone   chan struct{}
	createsReady chan struct{}
}

func (s *concurrentCreationStore) ExistsTask(ctx context.Context, id string) (bool, error) {
	exists, err := s.MemoryStore.ExistsTask(ctx, id)
	if s.checked.Add(1) == 2 {
		close(s.checksDone)
	}
	select {
	case <-s.checksDone:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return exists, err
}

func (s *concurrentCreationStore) CreateTask(ctx context.Context, task *types.Task) error {
	if s.creating.Add(1) == 2 {
		close(s.createsReady)
	}
	select {
	case <-s.createsReady:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.MemoryStore.CreateTask(ctx, task)
}

func TestCreateTaskConcurrentDuplicateCannotChangeTenant(t *testing.T) {
	workspace := "./testdata"
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = ""
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "fixture-a", WorkspaceRoot: workspace},
			"tenant-b": {APIKey: "fixture-b", WorkspaceRoot: workspace},
		}
	}))
	st := &concurrentCreationStore{MemoryStore: store.NewMemoryStore(), checksDone: make(chan struct{}), createsReady: make(chan struct{})}
	r := setupTestRouter(t, st, nil)
	type response struct {
		status       int
		tenant, goal string
	}
	results := make(chan response, 2)
	for _, suffix := range []string{"a", "b"} {
		go func() {
			goal := "goal-" + suffix
			body, _ := json.Marshal(map[string]any{"id": "same-id", "goal": goal, "workspace": workspace})
			req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(string(body)))
			req.Header.Set("X-API-Key", "fixture-"+suffix)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			results <- response{w.Code, "tenant-" + suffix, goal}
		}()
	}
	one, two := <-results, <-results
	winner, loser := one, two
	if two.status == http.StatusCreated {
		winner, loser = two, one
	}
	if winner.status != http.StatusCreated || loser.status != http.StatusConflict {
		t.Fatalf("concurrent responses: %+v %+v; want one 201 and one 409", one, two)
	}
	stored, err := st.GetTask(context.Background(), "same-id")
	if err != nil || stored.TenantID != winner.tenant || stored.Goal != winner.goal {
		t.Fatalf("winner overwritten: task=%+v err=%v", stored, err)
	}
}

// Embedding only Store intentionally hides optional atomic-create support.
type legacyCreationStore struct{ store.Store }

func TestCreateTaskFailsClosedWithoutAtomicStore(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "fixture-admin"
	}))
	base := store.NewMemoryStore()
	r := setupTestRouter(t, legacyCreationStore{base}, nil)
	workspace := "./testdata"
	body, _ := json.Marshal(map[string]any{"id": "unsupported", "goal": "fixture", "workspace": workspace})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fixture-admin")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501: %s", w.Code, w.Body.String())
	}
	if exists, err := base.ExistsTask(context.Background(), "unsupported"); err != nil || exists {
		t.Fatalf("unsafe fallback created task: exists=%v err=%v", exists, err)
	}
}
