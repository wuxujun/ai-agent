package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type faultyTaskExport struct {
	store.Store
	snapshot *store.TaskExportSnapshot
	err      error
}

func (f *faultyTaskExport) ReadTaskExport(context.Context, string, string, int) (*store.TaskExportSnapshot, error) {
	return f.snapshot, f.err
}
func TestTaskExportRejectsFaultyAdapterWithoutLeakingPayload(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.API.Tenants = map[string]config.APITenantConfig{"a": {}} }))
	for _, scenario := range []string{"foreign", "error", "unsupported", "gap", "unexpected-dependencies"} {
		t.Run(scenario, func(t *testing.T) {
			faulty := &faultyTaskExport{Store: store.NewMemoryStore(), snapshot: &store.TaskExportSnapshot{Task: &types.Task{ID: "task", TenantID: "b", Goal: "private-foreign-body"}}}
			var st store.Store = faulty
			want := 503
			switch scenario {
			case "error":
				faulty.err = errors.New("secret-upstream-token")
			case "unsupported":
				st = struct{ store.Store }{store.NewMemoryStore()}
				want = 501
			case "gap":
				faulty.snapshot.Task.TenantID = "a"
				faulty.snapshot.Traces = []store.TaskTraceEvent{{Sequence: 2}}
			case "unexpected-dependencies":
				faulty.snapshot.Task.TenantID = "a"
				faulty.snapshot.Dependencies = &store.TaskExportDependencies{}
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/retention/tasks/task/export?tenant_id=a", nil)
			c.Params = gin.Params{{Key: "id", Value: "task"}}
			setPrincipal(c, Principal{TenantID: "admin", Admin: true})
			(&Handler{store: st}).exportTask(c)
			if w.Code != want || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "secret-") {
				t.Fatalf("unsafe adapter result %d %s", w.Code, w.Body.String())
			}
		})
	}
}
