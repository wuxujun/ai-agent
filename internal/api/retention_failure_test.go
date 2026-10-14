package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

type faultyRetentionReader struct {
	store.Store
	rows []store.RetentionObservation
	err  error
}

func (s *faultyRetentionReader) ListTaskRetentionObservations(context.Context, store.TaskSummaryFilter) ([]store.RetentionObservation, error) {
	return s.rows, s.err
}
func TestRetentionAPIRejectsFaultyScopedObservationAndRedactsErrors(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.API.Tenants = map[string]config.APITenantConfig{"a": {}} }))
	for _, scenario := range []string{"foreign", "foreign-lookahead", "error", "unsupported"} {
		var st store.Store = &faultyRetentionReader{Store: store.NewMemoryStore(), rows: []store.RetentionObservation{{ID: "private-foreign-id", TenantID: "b", CreatedAt: time.Now()}}}
		if scenario == "foreign-lookahead" {
			st = &faultyRetentionReader{Store: store.NewMemoryStore(), rows: []store.RetentionObservation{{ID: "own", TenantID: "a", CreatedAt: time.Now()}, {ID: "private-foreign-id", TenantID: "b", CreatedAt: time.Now()}}}
		}
		if scenario == "error" {
			st = &faultyRetentionReader{Store: store.NewMemoryStore(), err: errors.New("upstream-private-credential")}
		}
		if scenario == "unsupported" {
			st = struct{ store.Store }{Store: store.NewMemoryStore()}
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/retention/tasks?tenant_id=a&limit=1", nil)
		setPrincipal(c, Principal{TenantID: "admin", Admin: true})
		(&Handler{store: st}).listRetentionAssessments(c)
		want := 503
		if scenario == "unsupported" {
			want = 501
		}
		if w.Code != want || strings.Contains(w.Body.String(), "upstream-private") || strings.Contains(w.Body.String(), "private-foreign") {
			t.Fatalf("%s: unsafe error contract: %d", scenario, w.Code)
		}
	}
}
