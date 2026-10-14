package api_test

import (
	"bytes"
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

func TestIsolatedRestoreAPI(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Tenants = map[string]config.APITenantConfig{"admin": {APIKey: "restore-admin", Admin: true}, "a": {APIKey: "restore-a"}, "b": {APIKey: "restore-b"}}
	}))
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, nil)
	task := &types.Task{ID: "restore", TenantID: "a", Status: types.StatusFailed, Goal: "private-goal", Trace: []types.StepTrace{{Observation: "private-trace"}}}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadJointTaskExport(t.Context(), task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.EncodeTaskExport(t.Context(), before, "memory", task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := func(query, key string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/retention/imports/verify?"+query, bytes.NewReader(body))
		req.Header.Set("X-API-Key", key)
		req.Header.Set("X-Admin", "true")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, key := range []string{"", "restore-a", "restore-b"} {
		w := request("tenant_id=a", ""+key, data)
		if w.Code != 401 && w.Code != 403 {
			t.Fatal("nonadmin restored", w.Code)
		}
	}
	for _, query := range []string{"", "tenant_id=unknown", "tenant_id=b", "tenant_id=a&tenant_id=b", "tenant_id=a&byte_budget=0", "tenant_id=a&byte_budget=8388609", "tenant_id=a&scope=joint", "tenant_id=a&broken=%xx"} {
		w := request(query, "restore-admin", data)
		if w.Code != 400 && w.Code != 404 {
			t.Fatal("invalid parameters accepted", query, w.Code)
		}
	}
	for _, body := range [][]byte{[]byte(`null`), []byte(`{"payload":"private-bad"}`), append(bytes.Clone(data), []byte(`{}`)...)} {
		w := request("tenant_id=a", "restore-admin", body)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private-") {
			t.Fatal("invalid body response", w.Code, w.Body.String())
		}
	}
	w := request("tenant_id=a&byte_budget=4096", "restore-admin", bytes.Repeat([]byte("x"), 4097))
	if w.Code != 413 {
		t.Fatal("body quota ignored", w.Code)
	}
	for range 2 {
		w := request("tenant_id=a", "restore-admin", data)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("restore failed", w.Code, w.Body.String())
		}
		var report store.TaskRestoreReport
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if !report.Verified || report.RestoreReady || !report.SourcePreserved || report.Target != "ephemeral_isolated_sqlite" || strings.Contains(w.Body.String(), "private-") {
			t.Fatal("wrong verification report", w.Body.String())
		}
	}
	after, err := st.ReadJointTaskExport(t.Context(), task.ID, "a", store.MaxTaskExportBytes)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("online source changed", err)
	}
}
