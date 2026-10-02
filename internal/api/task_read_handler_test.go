package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskReadAPI_SummaryTracePaginationAndTenantIsolation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = ""
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "task-read-a"},
			"tenant-b": {APIKey: "task-read-b"},
		}
	}))
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, &orchestrator.Engine{Store: st})
	created := time.Now().UTC().Truncate(time.Microsecond)
	for _, fixture := range []struct {
		id, tenant string
		at         time.Time
	}{
		{"a-1", "tenant-a", created}, {"a-2", "tenant-a", created},
		{"b-1", "tenant-b", created.Add(time.Hour)},
	} {
		if err := st.CreateTask(t.Context(), &types.Task{
			ID: fixture.id, TenantID: fixture.tenant, CreatedAt: fixture.at,
			Goal: "secret goal", Status: types.StatusCreated,
			Trace: []types.StepTrace{{Step: 1, Action: "one", Observation: "trace-secret"},
				{Step: 1, Action: "two"}, {Step: 2, Action: "three"}},
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
	first := request("/api/tasks?view=summary&limit=1", "task-read-a")
	var summary struct {
		Tasks      []struct{ ID string } `json:"tasks"`
		HasMore    bool                  `json:"has_more"`
		NextCursor string                `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &summary); err != nil || first.Code != http.StatusOK ||
		len(summary.Tasks) != 1 || summary.Tasks[0].ID != "a-2" || !summary.HasMore ||
		summary.NextCursor == "" || strings.Contains(first.Body.String(), "trace-secret") {
		t.Fatalf("summary first = %d %s, %v", first.Code, first.Body.String(), err)
	}
	second := request("/api/tasks?view=summary&limit=1&cursor="+url.QueryEscape(summary.NextCursor), "task-read-a")
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"id":"a-1"`) || strings.Contains(second.Body.String(), `"id":"b-1"`) {
		t.Fatalf("summary second = %d %s", second.Code, second.Body.String())
	}
	if wrongTenant := request("/api/tasks?view=summary&limit=1&cursor="+url.QueryEscape(summary.NextCursor), "task-read-b"); wrongTenant.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant cursor = %d", wrongTenant.Code)
	}
	detail := request("/api/tasks/a-1?view=summary", "task-read-a")
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), "trace-secret") || !strings.Contains(detail.Body.String(), `"allowed_actions":["run_all","delete"]`) {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	trace1 := request("/api/tasks/a-1/trace?limit=2", "task-read-a")
	var page struct {
		Events []struct {
			Sequence int64           `json:"sequence"`
			Trace    types.StepTrace `json:"trace"`
		} `json:"events"`
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(trace1.Body.Bytes(), &page); err != nil || trace1.Code != http.StatusOK ||
		len(page.Events) != 2 || page.Events[0].Sequence != 1 || page.Events[1].Sequence != 2 ||
		page.Events[1].Trace.Step != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("trace first = %d %s, %v", trace1.Code, trace1.Body.String(), err)
	}
	if !strings.Contains(trace1.Body.String(), `"event_id":"a-1:1"`) || !strings.Contains(trace1.Body.String(), `"recorded_at":`) {
		t.Fatalf("trace metadata missing = %s", trace1.Body.String())
	}
	trace2 := request("/api/tasks/a-1/trace?limit=2&cursor="+url.QueryEscape(page.NextCursor), "task-read-a")
	if trace2.Code != http.StatusOK || !strings.Contains(trace2.Body.String(), `"sequence":3`) || strings.Contains(trace2.Body.String(), `"sequence":2`) {
		t.Fatalf("trace second = %d %s", trace2.Code, trace2.Body.String())
	}
	if wrongTenant := request("/api/tasks/a-1/trace", "task-read-b"); wrongTenant.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant trace = %d", wrongTenant.Code)
	}
	if wrongTask := request("/api/tasks/a-2/trace?cursor="+url.QueryEscape(page.NextCursor), "task-read-a"); wrongTask.Code != http.StatusBadRequest {
		t.Fatalf("cross-task cursor = %d", wrongTask.Code)
	}
	legacy := request("/api/tasks/a-1", "task-read-a")
	if legacy.Code != http.StatusOK || !strings.Contains(legacy.Body.String(), "trace-secret") {
		t.Fatalf("legacy detail = %d %s", legacy.Code, legacy.Body.String())
	}
}

func TestTaskReadAPI_ResumablePartialTaskAction(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "task-resume-key"
	}))
	st := store.NewMemoryStore()
	engine := &orchestrator.Engine{Store: st, Mode: orchestrator.ModeMultiAgent}
	router := setupTestRouter(t, st, engine)
	if err := st.CreateTask(t.Context(), &types.Task{
		ID: "resumable-partial", TenantID: "default", Goal: "verify candidate",
		Status: types.StatusPartial, FinalAnswer: "persisted candidate",
		Trace: []types.StepTrace{{Action: "verify_draft", Observation: `{"version":1,"draft":{"final_answer":"persisted candidate"},"evidence":[],"execution_complete":true}`}},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/resumable-partial?view=summary", nil)
	req.Header.Set("X-API-Key", "task-resume-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"allowed_actions":["run_all","delete"]`) || strings.Contains(response.Body.String(), "verify_draft") {
		t.Fatalf("partial summary = %d %s", response.Code, response.Body.String())
	}
}
