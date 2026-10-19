package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/logger"
)

func TestErrorMiddlewareProtectsResponseAndLogs(t *testing.T) {
	dir := t.TempDir()
	if err := logger.Configure(logger.Options{Level: "info", FileEnabled: true, Directory: dir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close(); logger.Reinit("info") })
	r := gin.New()
	r.Use(AccessLogMiddleware(), ErrorMiddleware())
	const secret = "internal-secret-sentinel /private/database/path"
	r.GET("/error", func(c *gin.Context) { _ = c.Error(errors.New(secret)) })
	r.GET("/written", func(c *gin.Context) { c.String(202, "already written"); _ = c.Error(errors.New(secret)) })
	for _, tc := range []struct {
		path   string
		status int
	}{{"/error", 500}, {"/written", 202}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("X-Request-ID", "error-boundary-test")
		r.ServeHTTP(w, req)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "internal-secret-sentinel") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if tc.status == http.StatusInternalServerError && (!strings.Contains(w.Body.String(), `"code":"internal_server_error"`) || !strings.Contains(w.Body.String(), `"request_id":"error-boundary-test"`)) {
			t.Fatalf("missing correlation: %s", w.Body.String())
		}
		if tc.path == "/written" && w.Body.String() != "already written" {
			t.Fatalf("appended response: %s", w.Body.String())
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "internal-secret-sentinel") || strings.Contains(s, "/private/database/path") {
			t.Fatal("secret in log")
		}
		seen = seen || strings.Contains(s, "error-boundary-test")
	}
	if !seen {
		t.Fatal("missing correlated log")
	}
}
