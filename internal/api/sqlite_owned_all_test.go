package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/types"
)

func enableOwnedAll(t *testing.T, h *Handler) {
	t.Helper()
	if err := h.SetOwnedSQLiteRunAllAdapter(h.ownedSQLiteStep.options); err != nil {
		t.Fatal(err)
	}
}
func ownedAllRequest(router *gin.Engine, stream bool) *httptest.ResponseRecorder {
	path := "/api/tasks/owned-http/run-all"
	if stream {
		path += "?stream=true"
	}
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("X-API-Key", "owned-step-test-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
func TestOwnedSQLiteAllJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			router, h, st, p, _ := ownedStepFixture(t, nil)
			enableOwnedAll(t, h)
			w := ownedAllRequest(router, stream)
			h.Wait()
			want := http.StatusAccepted
			if stream {
				want = http.StatusOK
			}
			if w.Code != want || p.calls.Load() != 1 {
				t.Fatal(w.Code, w.Body.String(), p.calls.Load())
			}
			if stream && (!strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), `"status":"completed"`)) {
				t.Fatal(w.Header(), w.Body.String())
			}
			out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
			if err != nil || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
				t.Fatal("incomplete lifetime", err)
			}
		})
	}
}
func TestOwnedSQLiteAllBlockedBeforeResponse(t *testing.T) {
	router, h, st, p, epoch := ownedStepFixture(t, nil)
	enableOwnedAll(t, h)
	if _, err := st.BlockSQLiteExecutionGate(t.Context(), "owned-http", "default", epoch, false); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		w := ownedAllRequest(router, stream)
		if w.Code != http.StatusConflict || strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	h.Wait()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || len(out.Attempts) != 0 || p.calls.Load() != 0 {
		t.Fatal("blocked effects", err)
	}
}
func TestOwnedSQLiteAllQueuedCancelRetainsOwnerThroughCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	router, h, st, p, _ := ownedStepFixture(t, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	enableOwnedAll(t, h)
	// Saturate the existing semaphore without entering any Engine.
	h.taskSem = newResizableSemaphore(1)
	slots := 1
	if !h.taskSem.Acquire(t.Context()) {
		t.Fatal("reserve semaphore")
	}
	defer func() {
		for i := 0; i < slots; i++ {
			h.taskSem.Release()
		}
	}()
	w := ownedAllRequest(router, false)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	task, loadErr := st.GetTask(t.Context(), "owned-http")
	if err != nil || loadErr != nil || len(out.Attempts) != 1 || out.Attempts[0].State != "running" || task.Status != types.StatusRunning || p.calls.Load() != 0 {
		t.Fatal("202 before owned admission/CAS", err, loadErr)
	}
	if !h.CancelTaskByID("owned-http") {
		t.Fatal("queued run missing")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no queued cleanup")
	}
	h.activeTasksMu.Lock()
	retained := h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	out, err = st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if !retained || err != nil || out.Attempts[0].State != "exited" || out.LeaseState == "absent" {
		t.Fatal("owner released before cleanup", err)
	}
	close(release)
	h.Wait()
	out, err = st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || out.Attempts[0].State != "complete" || out.LeaseState != "absent" || p.calls.Load() != 0 {
		t.Fatal("queued abort not finalized", err)
	}
}

func TestOwnedSQLiteAllReloadRejectsChangedStatusBefore202(t *testing.T) {
	router, h, st, p, _ := ownedStepFixture(t, nil)
	provider := h.ownedSQLiteStep.options
	if err := h.SetOwnedSQLiteRunAllAdapter(func(ctx context.Context, id, tenant string) (orchestrator.SQLiteOwnedExecutionOptions, error) {
		// Change the checkpoint after the handler's initial load, before registration.
		task, err := st.GetTask(ctx, id)
		if err != nil {
			return orchestrator.SQLiteOwnedExecutionOptions{}, err
		}
		task.Status = types.StatusFailed
		if err := st.SaveFullTask(ctx, task); err != nil {
			return orchestrator.SQLiteOwnedExecutionOptions{}, err
		}
		return provider(ctx, id, tenant)
	}); err != nil {
		t.Fatal(err)
	}
	w := ownedAllRequest(router, false)
	h.Wait()
	saved, err := st.GetTask(t.Context(), "owned-http")
	out, snapErr := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if w.Code != http.StatusOK || err != nil || snapErr != nil || saved.Status != types.StatusFailed || p.calls.Load() != 0 || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
		t.Fatal("stale preflight checkpoint", w.Code, err, snapErr)
	}
}

func TestOwnedSQLiteAllSSEWaitsForCleanupAndCancelsDisconnect(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	router, h, st, _, _ := ownedStepFixture(t, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	enableOwnedAll(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/run-all?stream=true", nil).WithContext(ctx)
	req.Header.Set("X-API-Key", "owned-step-test-key")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); router.ServeHTTP(w, req); done <- w }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup not reached")
	}
	select {
	case <-done:
		t.Fatal("SSE finished before cleanup")
	default:
	}
	cancel()
	select {
	case w := <-done:
		if strings.Contains(w.Body.String(), `"status":"completed"`) {
			t.Fatal("premature terminal", w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect blocked")
	}
	h.activeTasksMu.Lock()
	retained := h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	if !retained {
		t.Fatal("disconnect removed cleanup owner")
	}
	close(release)
	h.Wait()
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
		t.Fatal("disconnect cleanup incomplete", err)
	}
}
