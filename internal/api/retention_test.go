package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type assessedPage struct {
	ReadOnly       bool `json:"read_only"`
	ExecutionReady bool `json:"execution_ready"`
	Items          []struct {
		ID         string   `json:"task_id"`
		Tenant     string   `json:"tenant_id"`
		Assessment string   `json:"assessment"`
		Reasons    []string `json:"reasons"`
	} `json:"items"`
	Count   int    `json:"count"`
	HasMore bool   `json:"has_more"`
	Cursor  string `json:"next_cursor"`
}

func TestRetentionAPIReadOnlyBudgetCursorAndAuthorization(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = ""
		c.API.Auth.Mode = "api_key"
		c.API.Tenants = map[string]config.APITenantConfig{"admin": {APIKey: "assessment-admin", Admin: true}, "a": {APIKey: "assessment-a"}, "b": {APIKey: "assessment-b"}}
	}))
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, &orchestrator.Engine{Store: st})
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < 60; i++ {
		if err := st.CreateTask(t.Context(), &types.Task{ID: fmt.Sprintf("row-%03d", i), TenantID: "a", Goal: "private-goal-sentinel", Status: types.StatusFailed, CreatedAt: now, Trace: []types.StepTrace{{Observation: "private-trace-sentinel"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTask(t.Context(), &types.Task{ID: "foreign-secret-id", TenantID: "b", Status: types.StatusFailed, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetTask(t.Context(), "row-000")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, key string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		r.Header.Set("X-Admin", "true")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, key := range []string{"", "assessment-a", "assessment-b"} {
		w := request("/api/retention/tasks?tenant_id=a", key)
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("non-admin accepted: %d", w.Code)
		}
	}
	for _, params := range []string{"", "tenant_id=missing", "tenant_id=a&limit=51", "tenant_id=a&limit=0", "tenant_id=a&hot_days=0", "tenant_id=a&hot_days=-1", "tenant_id=a&byte_budget=2", "tenant_id=a&dry_run=false", "tenant_id=a&tenant_id=b", "tenant_id=a&cursor=bad", "tenant_id=a&hold_task_id="} {
		w := request("/api/retention/tasks?"+params, "assessment-admin")
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("invalid parameters accepted (%s): %d", params, w.Code)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	firstCursor := ""
	for page := 0; page < 30; page++ {
		path := "/api/retention/tasks?tenant_id=a&limit=50&hot_days=30&byte_budget=4096"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		w := request(path, "assessment-admin")
		if w.Code != 200 || w.Body.Len() > 4096 {
			t.Fatalf("page/budget: %d size=%d %s", w.Code, w.Body.Len(), w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("assessment cache enabled")
		}
		body := w.Body.String()
		if strings.Contains(body, "private-") || strings.Contains(body, "foreign-secret") {
			t.Fatal("private payload/cross-tenant data leaked")
		}
		var p assessedPage
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if !p.ReadOnly || p.ExecutionReady || p.Count == 0 || p.Count != len(p.Items) {
			t.Fatal("assessment contract invalid")
		}
		for _, item := range p.Items {
			if item.Tenant != "a" || item.Assessment != "blocked" || seen[item.ID] || !strings.Contains(strings.Join(item.Reasons, ","), "retention_window_not_elapsed") {
				t.Fatal("unsafe or duplicate candidate")
			}
			seen[item.ID] = true
		}
		if !p.HasMore {
			break
		}
		if p.Cursor == "" {
			t.Fatal("budget-truncated page lacks cursor")
		}
		cursor = p.Cursor
		if firstCursor == "" {
			firstCursor = cursor
		}
	}
	if len(seen) != 60 {
		t.Fatalf("budget paging skipped records: %d", len(seen))
	}
	for _, prefix := range []string{"tenant_id=b&hot_days=30", "tenant_id=a&hot_days=31", "tenant_id=a&hot_days=30&hold_task_id=row-000"} {
		if w := request("/api/retention/tasks?"+prefix+"&cursor="+url.QueryEscape(firstCursor), "assessment-admin"); w.Code != 400 {
			t.Fatal("cursor replayed with different scope/policy")
		}
	}
	hold := request("/api/retention/tasks?tenant_id=a&limit=1&hold_task_id=row-059", "assessment-admin")
	if hold.Code != 200 || !strings.Contains(hold.Body.String(), `"assessment":"blocked"`) || !strings.Contains(hold.Body.String(), "requested_hold") {
		t.Fatal("explicit hold ignored")
	}
	after, err := st.GetTask(t.Context(), "row-000")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("assessment wrote task data")
	}
}
