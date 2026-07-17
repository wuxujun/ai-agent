package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type apiReauditPipeline struct{ calls int }

func (p *apiReauditPipeline) Process(_ context.Context, task *types.Task, _ string) (*types.AnswerAuditReport, error) {
	p.calls++
	report := &types.AnswerAuditReport{
		PipelineVersion: "p3-test", DraftHash: "draft", EvidenceHash: "evidence",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(), Enforcement: "strict", Publishable: true,
		FinalConfidence: "medium",
		Stages:          []types.AnswerAuditStage{{Name: "safety_guard_output", Status: "passed", Fingerprint: "safety-fp"}},
	}
	task.AnswerAudit = report
	return report, nil
}

func TestAuditEndpointsEnforceTenantScopeAndPersistReaudit(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "my-test-secret-api-key"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-audit-key"},
			"tenant-b": {APIKey: "tenant-b-audit-key"},
		}
		cfg.AnswerPipeline.Enabled = true
	}))
	st := store.NewMemoryStore()
	defer st.Close()
	oldReport := func(confidence string, publishable bool) *types.AnswerAuditReport {
		return &types.AnswerAuditReport{
			PipelineVersion: "p2-v1", Enforcement: "observe", Publishable: publishable, FinalConfidence: confidence,
			StartedAt: time.Now().Add(-time.Minute), CompletedAt: time.Now().Add(-time.Minute),
			Stages: []types.AnswerAuditStage{{Name: "fact_freshness_check", Status: "warned", Fingerprint: "fp"}},
		}
	}
	tasks := []*types.Task{
		{ID: "audit-a", TenantID: "tenant-a", Status: types.StatusCompleted, FinalAnswer: "answer a", AnswerAudit: oldReport("low", true)},
		{ID: "audit-b", TenantID: "tenant-b", Status: types.StatusCompleted, FinalAnswer: "answer b", AnswerAudit: oldReport("high", true)},
	}
	for _, task := range tasks {
		if err := st.SaveFullTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	pipeline := &apiReauditPipeline{}
	engine := &orchestrator.Engine{Mode: orchestrator.ModeLegacy, Store: st, AnswerPipeline: pipeline}
	router := setupTestRouter(t, st, engine)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/audits?tenant_id=tenant-b&confidence=low&limit=1", nil)
	req.Header.Set("X-API-Key", "tenant-a-audit-key")
	router.ServeHTTP(w, req)
	var list struct {
		Audits []struct {
			TaskID   string `json:"task_id"`
			TenantID string `json:"tenant_id"`
		} `json:"audits"`
		HasMore bool `json:"has_more"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Audits) != 1 || list.Audits[0].TaskID != "audit-a" || list.Audits[0].TenantID != "tenant-a" {
		t.Fatalf("tenant audit list: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/audits/summary", nil)
	req.Header.Set("X-API-Key", "tenant-a-audit-key")
	router.ServeHTTP(w, req)
	var summary struct {
		Eligible int     `json:"eligible_tasks"`
		Audited  int     `json:"audited_tasks"`
		Coverage float64 `json:"coverage_rate"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &summary) != nil || summary.Eligible != 1 || summary.Audited != 1 || summary.Coverage != 1 {
		t.Fatalf("tenant audit summary: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/tasks/audit-b/re-audit", strings.NewReader(`{"force":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-a-audit-key")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant re-audit code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/tasks/audit-a/re-audit", strings.NewReader(`{"force":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "tenant-a-audit-key")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || pipeline.calls != 1 {
		t.Fatalf("re-audit code=%d calls=%d body=%s", w.Code, pipeline.calls, w.Body.String())
	}
	persisted, err := st.GetTask(context.Background(), "audit-a")
	if err != nil || persisted.AnswerAudit == nil || persisted.AnswerAudit.PipelineVersion != "p3-test" {
		t.Fatalf("persisted task=%+v err=%v", persisted, err)
	}
}

func TestReauditRejectsNonTerminalTask(t *testing.T) {
	st := store.NewMemoryStore()
	defer st.Close()
	task := &types.Task{ID: "audit-running", Status: types.StatusRunning, FinalAnswer: "draft"}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	engine := &orchestrator.Engine{AnswerPipeline: &apiReauditPipeline{}, Store: st}
	router := setupTestRouter(t, st, engine)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/tasks/audit-running/re-audit", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}
