package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type failingSessionStore struct {
	*store.MemoryStore
	cause error
}

func (s *failingSessionStore) CreateSession(context.Context, *types.Session) error { return s.cause }

func TestSessionCreateErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{store.ErrSessionExists, 409}, {errors.New("database-secret"), 500}, {context.DeadlineExceeded, 500}} {
		h := &Handler{store: &failingSessionStore{MemoryStore: store.NewMemoryStore(), cause: tc.err}}
		r := gin.New()
		r.Use(ErrorMiddleware())
		r.POST("/sessions", func(c *gin.Context) { setPrincipal(c, Principal{TenantID: "tenant-a"}); h.createSession(c) })
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"title":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "database-secret") {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}

func TestSessionListPaginationContract(t *testing.T) {
	h := &Handler{store: store.NewMemoryStore()}
	r := gin.New()
	r.GET("/sessions", func(c *gin.Context) { setPrincipal(c, Principal{TenantID: "tenant-a"}); h.listSessions(c) })
	for _, tc := range []struct {
		query    string
		status   int
		fragment string
	}{
		{"", 200, `"limit":50`}, {"?limit=500", 200, `"limit":500`}, {"?limit=501", 400, ""}, {"?limit=-1", 400, ""}, {"?limit=abc", 400, ""}, {"?offset=-1", 400, ""}, {"?offset=abc", 400, ""}, {"?limit=1&offset=2", 200, `"offset":2`},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions"+tc.query, nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.fragment) {
			t.Fatalf("%s: %d %s", tc.query, w.Code, w.Body.String())
		}
	}
}
