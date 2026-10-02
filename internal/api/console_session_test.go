package api

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestConsoleSessionMiddleware_EncryptedCookieAndCSRF(t *testing.T) {
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 32)))
	encoded, err := encryptConsoleSession(consoleSession{
		Token: "bearer-secret-credential", TenantID: "tenant-a", CSRFToken: "csrf-fixture",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "bearer-secret-credential") {
		t.Fatal("session cookie contains plaintext credential")
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ConsoleSessionMiddleware())
	router.POST("/api/write", func(c *gin.Context) { c.String(http.StatusOK, c.GetHeader("Authorization")) })
	request := func(origin, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://example.test/api/write", nil)
		req.AddCookie(&http.Cookie{Name: consoleCookieName, Value: encoded})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-Console-CSRF", csrf)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if result := request("http://example.test", ""); result.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", result.Code)
	}
	if result := request("https://attacker.test", "csrf-fixture"); result.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d", result.Code)
	}
	if result := request("http://example.test", "csrf-fixture"); result.Code != http.StatusOK || result.Body.String() != "Bearer bearer-secret-credential" {
		t.Fatalf("valid session = %d %q", result.Code, result.Body.String())
	}
}

func TestConsoleAssets_SecurityHeadersAndKnownFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/console", serveConsoleIndex)
	router.GET("/assets/console/:name", serveConsoleAsset)
	for _, fixture := range []struct {
		path, contentType string
	}{
		{"/console", "text/html; charset=utf-8"},
		{"/assets/console/app.js", "text/javascript; charset=utf-8"},
		{"/assets/console/style.css", "text/css; charset=utf-8"},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixture.path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != fixture.contentType {
			t.Fatalf("%s: status=%d content-type=%q", fixture.path, response.Code, response.Header().Get("Content-Type"))
		}
		if fixture.path == "/console" && !strings.Contains(response.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("console index lacks script CSP")
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/console/unknown.js", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status = %d", response.Code)
	}
}
