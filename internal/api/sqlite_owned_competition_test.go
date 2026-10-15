package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestOwnedSQLiteCrossEntryCompetitionRetainsSingleOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	_, h, st, p, _ := ownedStepFixture(t, func(ctx context.Context, _ orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
		close(entered)
		select {
		case <-release:
			return strings.Repeat("c", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	exec := seedOwnedDispatcherApproval(t, h, true)
	staleApproval, err := st.GetApproval(t.Context(), "owned-decision", "default")
	if err != nil {
		t.Fatal(err)
	}
	h.startDurableApprovalRecovery("owned-http", "owned-decision")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not reach cleanup")
	}
	// A separate Handler has a different active map, so rejection here must come
	// from the durable protocol rather than an in-process reservation collision.
	peerEngine := &orchestrator.Engine{Mode: orchestrator.ModeLegacy, Store: st, ExecutionAdmission: st, Planner: p, ApprovalCodec: h.engine.ApprovalCodec, Executor: exec}
	router := gin.New()
	peer := RegisterRoutes(router, st, peerEngine, nil)
	t.Cleanup(peer.Wait)
	if err := peer.SetOwnedSQLiteStepAdapter(h.ownedSQLiteStep.options); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetOwnedSQLiteRunAllAdapter(h.ownedSQLiteStep.options); err != nil {
		t.Fatal(err)
	}
	if err := peerEngine.SetOwnedSQLiteApprovalDispatcher(orchestrator.SQLiteOwnedApprovalOptionsProvider(h.ownedSQLiteStep.options)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"run", "run?stream=true", "run-all", "run-all?stream=true"} {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/"+path, nil)
		req.Header.Set("X-API-Key", "owned-step-test-key")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("peer %s bypassed durable owner: %d %s", path, w.Code, w.Body.String())
		}
	}
	task, err := st.GetTask(t.Context(), "owned-http")
	if err != nil {
		t.Fatal(err)
	}
	if err := peerEngine.Next(t.Context(), task); !errors.Is(err, types.ErrExecutionAdmission) {
		t.Fatal("direct Engine bypassed owner", err)
	}
	recovered, err := peerEngine.DispatchApprovalRecovery(t.Context(), task, staleApproval, "startup-peer")
	if recovered || !errors.Is(err, store.ErrSQLiteExecutionConflict) {
		t.Fatal("peer dispatcher bypassed owner", err)
	}
	// A timed-out Shutdown cannot remove the cleanup owner or perform legacy rollback.
	shutdownCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = h.Shutdown(shutdownCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown did not time out", err)
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	h.activeTasksMu.Lock()
	retained := h.activeTasks["owned-http"] != nil
	h.activeTasksMu.Unlock()
	if err != nil || !retained || len(out.Attempts) != 1 || out.Attempts[0].State != "exited" || out.LeaseState == "absent" || p.calls.Load() != 0 || exec.calls.Load() != 1 {
		t.Fatal("competition changed owner", err)
	}
	close(release)
	h.Wait()
	peer.Wait()
	out, err = st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
		t.Fatal("original owner could not finalize", err)
	}
}

func TestOwnedSQLiteSSECleanupFailureDoesNotPublishSuccess(t *testing.T) {
	for _, entry := range []string{"run", "run-all"} {
		for _, failure := range []string{"error", "panic"} {
			t.Run(entry+"/"+failure, func(t *testing.T) {
				router, h, st, _, _ := ownedStepFixture(t, func(context.Context, orchestrator.SQLiteOwnedExecutionIdentity) (string, error) {
					if failure == "panic" {
						panic("fixture cleanup panic")
					}
					return "", errors.New("secret-cleanup-error")
				})
				enableOwnedAll(t, h)
				req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/"+entry+"?stream=true", nil)
				req.Header.Set("X-API-Key", "owned-step-test-key")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				h.Wait()
				if strings.Contains(w.Body.String(), `"status":"completed"`) || strings.Contains(w.Body.String(), "secret-cleanup") || strings.Contains(w.Body.String(), "fixture cleanup panic") {
					t.Fatal("cleanup failure published success or private detail", w.Body.String())
				}
				out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
				if err != nil || len(out.Attempts) != 1 || out.Attempts[0].State != "exited" || out.Attempts[0].FlushSHA256 != "" || out.Attempts[0].CleanupSHA256 != "" || out.LeaseState == "absent" || out.ReceiptsComplete {
					t.Fatal("cleanup failure finalized", err)
				}
			})
		}
	}
}

type ownedCompetitionPlanner struct {
	calls   atomic.Int32
	release <-chan struct{}
}

func (p *ownedCompetitionPlanner) PlanNext(ctx context.Context, _ *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	p.calls.Add(1)
	select {
	case <-p.release:
		return &planner.PlanDecision{Stop: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestOwnedSQLiteConcurrentStepAndRunAllOneWinner(t *testing.T) {
	router, h, st, _, _ := ownedStepFixture(t, nil)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	p := &ownedCompetitionPlanner{release: release}
	h.engine.Planner = p
	peerEngine := &orchestrator.Engine{Mode: orchestrator.ModeLegacy, Store: st, ExecutionAdmission: st, Planner: p}
	peerRouter := gin.New()
	peer := RegisterRoutes(peerRouter, st, peerEngine, nil)
	t.Cleanup(peer.Wait)
	if err := peer.SetOwnedSQLiteRunAllAdapter(h.ownedSQLiteStep.options); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, entry := range []struct {
		router *gin.Engine
		path   string
	}{{router, "run"}, {peerRouter, "run-all"}} {
		go func() {
			<-ready
			req := httptest.NewRequest(http.MethodPost, "/api/tasks/owned-http/"+entry.path, nil)
			req.Header.Set("X-API-Key", "owned-step-test-key")
			w := httptest.NewRecorder()
			entry.router.ServeHTTP(w, req)
			responses <- w
		}()
	}
	close(ready)
	read := func() *httptest.ResponseRecorder {
		select {
		case w := <-responses:
			return w
		case <-time.After(5 * time.Second):
			unblock()
			t.Fatal("competition response stalled")
			return nil
		}
	}
	first := read()
	if first.Code == http.StatusConflict {
		unblock()
	}
	second := read()
	unblock()
	h.Wait()
	peer.Wait()
	conflicts := 0
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		if w.Code == http.StatusConflict {
			conflicts++
		} else if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
			t.Fatal("unexpected response", w.Code, w.Body.String())
		}
	}
	out, err := st.ReadSQLiteExecutionGateSnapshot(t.Context(), "owned-http", "default")
	if err != nil || conflicts != 1 || p.calls.Load() != 1 || len(out.Attempts) != 1 || out.Attempts[0].State != "complete" || out.LeaseState != "absent" {
		t.Fatal("concurrent entries did not have one owner", err, conflicts, p.calls.Load())
	}
}
