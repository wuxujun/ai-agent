package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskWorkflowAPI_VerifiedGraphAndTenantIsolation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = ""
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "workflow-a"},
			"tenant-b": {APIKey: "workflow-b"},
		}
	}))
	graph, err := multiagent.BuildWorkflowGraph(multiagent.WorkflowReviewed)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := graph.Summary()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := multiagent.WorkflowRuntimeCheckpoint{
		Version: 1, Workflow: multiagent.WorkflowReviewed, Route: multiagent.WorkflowReviewed,
		GraphDigest: summary.Digest, States: map[string]multiagent.WorkflowNodeState{
			"plan": multiagent.WorkflowNodeSucceeded, "critique": multiagent.WorkflowNodeFailed,
		},
		Results: map[string]multiagent.WorkflowNodeResult{"plan": {Data: json.RawMessage(`{"secret":"private result"}`)}},
		Errors:  map[string]string{"critique": "private upstream error"},
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, nil)
	for _, task := range []*types.Task{
		{ID: "dag-task", TenantID: "tenant-a", Mode: "multiagent", Status: types.StatusFailed,
			Trace: []types.StepTrace{{Action: multiagent.WorkflowRuntimeCheckpointTraceAction, Observation: string(encoded)}}},
		{ID: "legacy-task", TenantID: "tenant-a", Mode: "multiagent", Status: types.StatusCompleted},
	} {
		if err := st.CreateTask(t.Context(), task); err != nil {
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
	owned := request("/api/tasks/dag-task/workflow", "workflow-a")
	var response struct {
		Available bool                         `json:"available"`
		Graph     multiagent.WorkflowGraphView `json:"graph"`
	}
	if err := json.Unmarshal(owned.Body.Bytes(), &response); err != nil || owned.Code != http.StatusOK ||
		!response.Available || response.Graph.GraphDigest != summary.Digest || len(response.Graph.Levels) != 4 ||
		response.Graph.Levels[1][0].State != multiagent.WorkflowNodeFailed {
		t.Fatalf("graph = %d %s, %v", owned.Code, owned.Body.String(), err)
	}
	if strings.Contains(owned.Body.String(), "private result") || strings.Contains(owned.Body.String(), "private upstream error") {
		t.Fatalf("workflow response leaked checkpoint payload: %s", owned.Body.String())
	}
	if legacy := request("/api/tasks/legacy-task/workflow", "workflow-a"); legacy.Code != http.StatusOK || legacy.Body.String() != `{"available":false}` {
		t.Fatalf("legacy graph = %d %s", legacy.Code, legacy.Body.String())
	}
	if foreign := request("/api/tasks/dag-task/workflow", "workflow-b"); foreign.Code != http.StatusNotFound || strings.Contains(foreign.Body.String(), summary.Digest) {
		t.Fatalf("foreign graph = %d %s", foreign.Code, foreign.Body.String())
	}
}
