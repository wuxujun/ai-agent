package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
)

func TestRouterTrustsOnlyConfiguredProxies(t *testing.T) {
	for _, tc := range []struct {
		proxies      []string
		remote, want string
	}{
		{nil, "127.0.0.1:1234", "127.0.0.1"},
		{[]string{"127.0.0.1"}, "127.0.0.1:1234", "203.0.113.42"},
		{[]string{"127.0.0.1"}, "192.0.2.10:1234", "192.0.2.10"},
	} {
		cfg := &config.Config{}
		cfg.API.TrustedProxies = tc.proxies
		r := newAPIRouter(cfg)
		r.GET("/ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
		req := httptest.NewRequest("GET", "/ip", nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", "203.0.113.42")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Body.String() != tc.want {
			t.Fatalf("proxies=%v remote=%s got=%s want=%s", tc.proxies, tc.remote, w.Body.String(), tc.want)
		}
	}
}
