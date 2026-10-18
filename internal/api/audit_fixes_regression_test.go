package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestOrphanCancelWithClusterBusUsesDurableFallback(t *testing.T) {
	st := store.NewMemoryStore()
	defer st.Close()
	task := &types.Task{ID: "task", TenantID: "tenant", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	record := &types.DurableApproval{ID: "approval", TaskID: task.ID, TenantID: task.TenantID, Status: types.ApprovalPending, ActionPayload: []byte("fixture"), Request: types.ApprovalRequest{ID: "approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}}
	if err := st.CreateApproval(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	// A bus with no client must never be called when no live peer lease exists.
	h := &Handler{store: st, approvalBus: orchestrator.NewApprovalBus(nil)}
	w, _ := orphanCancelRequest(h)
	saved, err := st.GetTask(t.Context(), task.ID)
	if w.Code != http.StatusOK || err != nil || saved.TerminationKind != types.TerminationUserCancelled {
		t.Fatalf("cancel=%d task=%+v err=%v", w.Code, saved, err)
	}
	approval, err := st.GetApproval(t.Context(), record.ID, record.TenantID)
	if err != nil || approval.Status != types.ApprovalExpired {
		t.Fatalf("approval=%+v err=%v", approval, err)
	}
}

type shutdownRecoveryStore struct {
	*store.MemoryStore
	entered chan struct{}
}

func (s *shutdownRecoveryStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestOrdinaryApprovalRecoveryStopsWithHandler(t *testing.T) {
	st := &shutdownRecoveryStore{MemoryStore: store.NewMemoryStore(), entered: make(chan struct{})}
	defer st.Close()
	h := &Handler{store: st, engine: &orchestrator.Engine{}}
	h.startDurableApprovalRecovery("task", "approval")
	select {
	case <-st.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	// Admission must also remain closed after Shutdown, without adding to WaitGroup.
	h.startDurableApprovalRecovery("task", "new-approval")
	h.Wait()
	h.activeTasksMu.Lock()
	defer h.activeTasksMu.Unlock()
	if len(h.recoveryWorkers) != 0 {
		t.Fatal("recovery survived shutdown")
	}
}

func TestJWKSDefaultTransportRejectsPrivateEndpoint(t *testing.T) {
	cfg := testJWTConfig()
	cfg.JWKSURL = "https://127.0.0.1:1/jwks"
	// verifierFor constructs the production client; no test transport override.
	_, err := verifierFor(cfg).fetchKeys(t.Context())
	if err == nil {
		t.Fatal("private JWKS endpoint accepted")
	}
}

type auditProjectionOnlyStore struct{ *store.SQLiteStore }

func (s *auditProjectionOnlyStore) ListTasks(context.Context, store.ListFilter) ([]*types.Task, error) {
	return nil, errors.New("full-task audit scan forbidden")
}

func TestAuditEndpointsUseSQLProjectionAndPreserveTenantCoverage(t *testing.T) {
	st, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, task := range []*types.Task{
		{ID: "a", TenantID: "tenant", Status: types.StatusCompleted, FinalAnswer: "answer", AnswerAudit: &types.AnswerAuditReport{FinalConfidence: "high", Publishable: true}},
		{ID: "b", TenantID: "tenant", Status: types.StatusCompleted, FinalAnswer: "answer"},
		{ID: "c", TenantID: "other", Status: types.StatusCompleted, FinalAnswer: "answer", AnswerAudit: &types.AnswerAuditReport{FinalConfidence: "high", Publishable: true}},
	} {
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{store: &auditProjectionOnlyStore{st}}
	request := func(path string, handler func(*gin.Context)) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		c.Request = req
		c.Set(principalKey, Principal{TenantID: "tenant"})
		handler(c)
		if w.Code != http.StatusOK || len(c.Errors) > 0 {
			t.Fatalf("response=%d errors=%v body=%s", w.Code, c.Errors, w.Body.String())
		}
		return w
	}
	w := request("/api/audits?limit=1&confidence=high", h.listAudits)
	var list struct {
		Audits  []auditListItem `json:"audits"`
		HasMore bool            `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Audits) != 1 || list.Audits[0].TaskID != "a" || list.HasMore {
		t.Fatalf("list=%s err=%v", w.Body.String(), err)
	}
	w = request("/api/audits/summary?confidence=high", h.getAuditSummary)
	var summary struct {
		Eligible int     `json:"eligible_tasks"`
		Audited  int     `json:"audited_tasks"`
		Coverage float64 `json:"coverage_rate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil || summary.Eligible != 2 || summary.Audited != 1 || summary.Coverage != 0.5 {
		t.Fatalf("summary=%s err=%v", w.Body.String(), err)
	}
}
