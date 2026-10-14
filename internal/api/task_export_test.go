package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskExportAPIAuthScopeBudgetAndSourcePreservation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Tenants = map[string]config.APITenantConfig{"admin": {APIKey: "export-admin", Admin: true}, "a": {APIKey: "export-a"}, "b": {APIKey: "export-b"}}
	}))
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, nil)
	task := &types.Task{ID: "export", TenantID: "a", Status: types.StatusFailed, Goal: "private-goal", Trace: []types.StepTrace{{Observation: "private-trace"}}}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetTask(t.Context(), task.ID)
	request := func(params, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/retention/tasks/export/export?"+params, nil)
		req.Header.Set("X-API-Key", key)
		req.Header.Set("X-Admin", "true")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, key := range []string{"", "export-a", "export-b"} {
		w := request("tenant_id=a", key)
		if w.Code != 401 && w.Code != 403 {
			t.Fatal("nonadmin exported", w.Code)
		}
	}
	for _, params := range []string{"", "tenant_id=missing", "tenant_id=b", "tenant_id=a&tenant_id=b", "tenant_id=a&byte_budget=0", "tenant_id=a&byte_budget=8388609", "tenant_id=a&delete=true", "tenant_id=a&byte_budget=4096&byte_budget=8192", "tenant_id=a&broken=%xx"} {
		w := request(params, "export-admin")
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("invalid request accepted %s %d", params, w.Code)
		}
		if strings.Contains(w.Body.String(), "private-") {
			t.Fatal("payload leaked on failure")
		}
	}
	w := request("tenant_id=a&byte_budget=4096", "export-admin")
	if w.Code != 200 || w.Body.Len() > 4096 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") != `attachment; filename="task-export.json"` {
		t.Fatalf("export %d %s", w.Code, w.Body.String())
	}
	var artifact store.TaskExportArtifact
	if err := json.Unmarshal(w.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(artifact.Payload)
	if artifact.PayloadSHA256 != hex.EncodeToString(hash[:]) || artifact.RestoreReady || !artifact.SourcePreserved || !strings.Contains(string(artifact.Payload), "private-trace") {
		t.Fatal("artifact invalid")
	}
	after, _ := st.GetTask(t.Context(), task.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("export changed task")
	}
	if w := request("tenant_id=a&scope=unknown", "export-admin"); w.Code != 400 {
		t.Fatal("unknown scope accepted")
	}
	if w := request("tenant_id=a&scope=joint&scope=task_trace", "export-admin"); w.Code != 400 {
		t.Fatal("duplicate scope accepted")
	}
	joint := request("tenant_id=a&scope=joint&byte_budget=4096", "export-admin")
	if joint.Code != 200 {
		t.Fatalf("joint export: %d %s", joint.Code, joint.Body.String())
	}
	var jointArtifact store.TaskExportArtifact
	if err := json.Unmarshal(joint.Body.Bytes(), &jointArtifact); err != nil {
		t.Fatal(err)
	}
	if jointArtifact.Schema != "task-joint-export-v2" || jointArtifact.RestoreReady || !strings.Contains(string(jointArtifact.Payload), `"dependencies"`) {
		t.Fatal("joint schema not selected")
	}
	task.Goal = strings.Repeat("x", 20000)
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if w := request("tenant_id=a&byte_budget=4096", "export-admin"); w.Code != 413 || strings.Contains(w.Body.String(), "xxxxx") {
		t.Fatal("oversize leaked or truncated", w.Code)
	}
}
