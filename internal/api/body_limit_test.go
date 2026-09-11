package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestRequestBodyLimitRejectsOversizedJSON(t *testing.T) {
	router := gin.New()
	RegisterRoutes(router, store.NewMemoryStore(), &orchestrator.Engine{}, nil)
	router.POST("/body-fixture", func(c *gin.Context) {
		var body struct {
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Error(err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name    string
		bytes   int
		unknown bool
		status  int
	}{
		{"small", 16, false, 204}, {"declared_large", 10 << 20, false, 413}, {"chunked_large", 10 << 20, true, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/body-fixture", strings.NewReader(`{"value":"`+strings.Repeat("x", tc.bytes)+`"}`))
			request.Header.Set("Content-Type", "application/json")
			if tc.unknown {
				request.ContentLength = -1
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d want %d", response.Code, tc.status)
			}
		})
	}
}
