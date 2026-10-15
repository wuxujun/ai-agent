package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/trace"
)

type ownedStepPlanner struct{ calls atomic.Int32 }

func (p *ownedStepPlanner) PlanNext(context.Context, *types.Task, func(string)) (*planner.PlanDecision, error) {
	p.calls.Add(1)
	return &planner.PlanDecision{Stop: true}, nil
}
func ownedStepFixture(t *testing.T, cleanup func(context.Context, orchestrator.SQLiteOwnedExecutionIdentity) (string, error)) (*gin.Engine, *Handler, *store.SQLiteStore, *ownedStepPlanner, int64) {
	t.Helper()
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = "owned-step-test-key"
		c.API.Auth.Mode = "api_key"
		c.API.Tenants = map[string]config.APITenantConfig{"other": {APIKey: "owned-other-test-key"}}
	}))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewSQLiteStoreWithExecutionGate(filepath.Join(dir, "http.sqlite"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	task := &types.Task{ID: "owned-http", TenantID: "default", Status: types.StatusRunning, Goal: "fixture", StepCount: 1, MaxSteps: 3, ToolBudget: 3}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	epoch, err := st.EnrollSQLiteExecutionGate(t.Context(), task.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	p := &ownedStepPlanner{}
	e := &orchestrator.Engine{Mode: orchestrator.ModeLegacy, Store: st, ExecutionAdmission: st, Planner: p}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := RegisterRoutes(router, st, e, nil)
	t.Cleanup(h.Wait)
	if cleanup == nil {
		cleanup = func(context.Context, orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
			return strings.Repeat("c", 64), nil
		}
	}
	if err := h.SetOwnedSQLiteStepAdapter(func(ctx context.Context, id, tenant string) (orchestrator.SQLiteOwnedExecutionOptions, error) {
		return orchestrator.SQLiteOwnedExecutionOptions{AttemptID: uuid.NewString(), InstanceSHA256: strings.Repeat("b", 64), ExpectedGateEpoch: epoch, TTL: 3 * time.Second, Cleanup: cleanup}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return router, h, st, p, epoch
}
func ownedStepRequest(router *gin.Engine, stream, authorized bool) *httptest.ResponseRecorder {
	path := "/api/tasks/owned-http/run"
	if stream {
		path += "?stream=true"
	}
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if authorized {
		req.Header.Set("X-API-Key", "owned-step-test-key")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
func TestOwnedSQLiteStepJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			router, h, st, p, _ := ownedStepFixture(t, nil)
			w := ownedStepRequest(router, stream, true)
			h.Wait()
			if w.Code != http.StatusOK || p.calls.Load() != 1 {
				t.Fatal("owned HTTP execution", w.Code, w.Body.String())
			}
			if stream && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal("SSE became JSON", w.Header())
			}
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
			if err != nil || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
				t.Fatal("HTTP lifetime incomplete", err)
			}
		})
	}
}
func TestOwnedSQLiteStepAuthenticationAndBlockedGate(t *testing.T) {
	router, h, st, p, epoch := ownedStepFixture(t, nil)
	if w := ownedStepRequest(router, false, false); w.Code != http.StatusUnauthorized {
		t.Fatal("auth bypass", w.Code)
	}
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "owned-http", "default", epoch, false); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		w := ownedStepRequest(router, stream, true)
		if w.Code != http.StatusConflict {
			t.Fatal("blocked gate", w.Code, w.Body.String())
		}
	}
	h.Wait()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || len(out.Attempts) != 0 || p.calls.Load() != 0 {
		t.Fatal("rejected request had effects", err)
	}
}
func TestOwnedSQLiteStepCancelKeepsActiveUntilCleanup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	router, h, st, _, _ := ownedStepFixture(t, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- ownedStepRequest(router, false, true) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup not reached")
	}
	if !h.CancelTaskByID("owned-http") {
		t.Fatal("owned task absent before cleanup")
	}
	h.activeTasksMu.Lock()
	retained := h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	if !retained {
		t.Fatal("cancel hid unresolved owned worker")
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || out.Attempts[0].State != "exited" || out.LeaseState == "absent" {
		t.Fatal("cancel finalized prematurely", err)
	}
	close(release)
	h.Wait()
	if response := <-done; response.Code < 400 {
		t.Fatal("cancel returned success", response.Code)
	}
	h.activeTasksMu.Lock()
	retained = h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	if retained {
		t.Fatal("completed owned worker leaked slot")
	}
}

func TestOwnedSQLiteStepPreservesRequestTrace(t *testing.T) {
	router, h, st, _, _ := ownedStepFixture(t, nil)
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	scope := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/run", nil)
	req = req.WithContext(trace.ContextWithSpanContext(req.Context(), scope))
	req.Header.Set("X-API-Key", "owned-step-test-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	h.Wait()
	task, err := st.GetTask(t.Context(), "owned-http")
	if err != nil || w.Code != http.StatusOK || task.ExecutionTraceID != traceID.String() {
		t.Fatal("request trace lost", err, w.Code)
	}
}

func TestOwnedSQLiteStepRetainsTenantIsolation(t *testing.T) {
	router, h, st, p, _ := ownedStepFixture(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/run", nil)
	req.Header.Set("X-API-Key", "owned-other-test-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	h.Wait()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if w.Code != http.StatusNotFound || err != nil || len(out.Attempts) != 0 || p.calls.Load() != 0 {
		t.Fatal("tenant scope bypass", w.Code, err)
	}
}
