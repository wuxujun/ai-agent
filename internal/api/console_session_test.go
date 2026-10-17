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
	"github.com/wuxujun/ai-agent/internal/config"
)

func TestCreateConsoleSession_UATokenCookie(t *testing.T) {
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 32)))
	introspection := testIntrospectionConfig()
	introspection.CacheTTLSeconds = 0
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: introspection}
		cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {}}
	}))
	installTestIntrospector(t, introspection, nil, func(req *http.Request) (int, string) {
		if req.Header.Get("Authorization") != "Bearer cookie-fixture-token" {
			return http.StatusUnauthorized, `{}`
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"tenant-a","aud":"ai-agent","exp":` + expirationAfter(time.Hour) + `}}`
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/console/session", createConsoleSession)
	for _, tc := range []struct {
		name, body, cookie, origin string
		want                       int
	}{
		{name: "cookie", body: `{}`, cookie: "cookie-fixture-token", want: http.StatusOK},
		{name: "empty body token", body: `{"token":" "}`, cookie: "cookie-fixture-token", want: http.StatusOK},
		{name: "encoded cookie", body: `{}`, cookie: "cookie-fixture%2Dtoken", want: http.StatusOK},
		{name: "manual takes precedence", body: `{"token":"cookie-fixture-token"}`, cookie: "invalid", want: http.StatusOK},
		{name: "invalid manual does not fall back", body: `{"token":"invalid"}`, cookie: "cookie-fixture-token", want: http.StatusUnauthorized},
		{name: "invalid cookie", body: `{}`, cookie: "invalid", want: http.StatusUnauthorized},
		{name: "missing cookie", body: `{}`, want: http.StatusBadRequest},
		{name: "empty cookie", body: `{}`, cookie: " ", want: http.StatusBadRequest},
		{name: "oversized cookie", body: `{}`, cookie: strings.Repeat("a", 2501), want: http.StatusBadRequest},
		{name: "foreign origin", body: `{}`, cookie: "cookie-fixture-token", origin: "https://foreign.invalid", want: http.StatusForbidden},
		{name: "malformed body", body: `{`, cookie: "cookie-fixture-token", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://example.test/console/session", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "UA_TOKEN", Value: tc.cookie})
			}
			req.Header.Set("Origin", tc.origin)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d", response.Code, tc.want)
			}
			if tc.want == http.StatusOK {
				cookies := response.Result().Cookies()
				if len(cookies) != 1 || cookies[0].Name != consoleCookieName || !cookies[0].HttpOnly {
					t.Fatal("expected encrypted HttpOnly console session")
				}
				check := httptest.NewRequest(http.MethodGet, "/", nil)
				check.AddCookie(cookies[0])
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = check
				session, err := readConsoleSession(ctx)
				if err != nil || session.Token != "cookie-fixture-token" || session.TenantID != "tenant-a" {
					t.Fatal("session did not retain the verified credential and tenant")
				}
			} else if len(response.Result().Cookies()) != 0 {
				t.Fatal("failed authentication created a session cookie")
			}
		})
	}
}

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
