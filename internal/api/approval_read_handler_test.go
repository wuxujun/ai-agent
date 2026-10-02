package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestApprovalReadAPI_TenantPaginationAndSafeProjection(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = ""
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-a-approval-key"},
			"tenant-b": {APIKey: "tenant-b-approval-key"},
		}
	}))
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, nil)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := st.CreateTask(t.Context(), &types.Task{ID: tenant + "-task", TenantID: tenant, Goal: "test", Status: types.StatusAwaitingApproval}); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct{ id, tenant string }{{"a-1", "tenant-a"}, {"a-2", "tenant-a"}, {"b-1", "tenant-b"}} {
		if err := st.CreateApproval(t.Context(), &types.DurableApproval{
			ID: fixture.id, TaskID: fixture.tenant + "-task", TenantID: fixture.tenant,
			Request: types.ApprovalRequest{ID: fixture.id, TaskID: fixture.tenant + "-task", Action: "write_file", RiskLevel: types.RiskLevelHigh,
				Preview: "token: secret-value", Parameters: map[string]any{"raw": "must-not-appear"}},
			ActionPayload: []byte("ciphertext-secret"), ResolutionPayload: []byte("resolution-secret"), Status: types.ApprovalPending,
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := func(path, key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-API-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	first := request("/api/approvals?limit=1", "tenant-a-approval-key")
	if first.Code != http.StatusOK || strings.Contains(first.Body.String(), "ciphertext-secret") || strings.Contains(first.Body.String(), "must-not-appear") || strings.Contains(first.Body.String(), "secret-value") {
		t.Fatalf("first page = %d %s", first.Code, first.Body.String())
	}
	var page struct {
		Approvals  []struct{ ID string } `json:"approvals"`
		HasMore    bool                  `json:"has_more"`
		NextCursor string                `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || len(page.Approvals) != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	second := request("/api/approvals?limit=1&cursor="+page.NextCursor, "tenant-a-approval-key")
	if second.Code != http.StatusOK || strings.Contains(second.Body.String(), page.Approvals[0].ID) || strings.Contains(second.Body.String(), "b-1") {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
	if other := request("/api/approvals?limit=1&cursor="+page.NextCursor, "tenant-b-approval-key"); other.Code != http.StatusBadRequest {
		t.Fatalf("foreign cursor status = %d", other.Code)
	}
	if other := request("/api/approvals/a-1", "tenant-b-approval-key"); other.Code != http.StatusNotFound {
		t.Fatalf("foreign approval status = %d", other.Code)
	}
	if other := request("/api/tasks/tenant-a-task/approvals", "tenant-b-approval-key"); other.Code != http.StatusNotFound {
		t.Fatalf("foreign task approvals status = %d", other.Code)
	}
	if current := request("/api/tasks/tenant-a-task/approvals?status=pending", "tenant-a-approval-key"); current.Code != http.StatusOK || strings.Contains(current.Body.String(), "ciphertext-secret") {
		t.Fatalf("task approvals = %d %s", current.Code, current.Body.String())
	}
}

func TestApprovalReadAPI_EncryptedDecisionActor(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "approval-audit-key"
	}))
	st := store.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x48}, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine := &orchestrator.Engine{Store: st, ApprovalCodec: codec}
	if err := st.CreateTask(t.Context(), &types.Task{ID: "actor-task", TenantID: "default", Goal: "test", Status: types.StatusAwaitingApproval}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateApproval(t.Context(), &types.DurableApproval{
		ID: "actor-approval", TaskID: "actor-task", TenantID: "default",
		Request:       types.ApprovalRequest{ID: "actor-approval", TaskID: "actor-task", Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("opaque-encrypted-action"), Status: types.ApprovalPending,
	}); err != nil {
		t.Fatal(err)
	}
	_, found, persisted, err := engine.PersistApprovalResolution(t.Context(), "actor-task", "actor-approval", types.ApprovalResult{Approved: true, ActorID: "jwt-sub:abc123"})
	if err != nil || !found || !persisted {
		t.Fatalf("persist decision = found=%v persisted=%v err=%v", found, persisted, err)
	}
	router := setupTestRouter(t, st, engine)
	req := httptest.NewRequest(http.MethodGet, "/api/approvals/actor-approval", nil)
	req.Header.Set("X-API-Key", "approval-audit-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"actor_id":"jwt-sub:abc123"`) || strings.Contains(response.Body.String(), "opaque-encrypted-action") {
		t.Fatalf("approval response = %d %s", response.Code, response.Body.String())
	}
}
