package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestTenantPromptInitAcceptsAuthenticatedDynamicTenant(t *testing.T) {
	previousMode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 32)))
	introspection := testIntrospectionConfig()
	introspection.RequireKnownTenant = false
	introspection.CacheTTLSeconds = 0
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: introspection}
		cfg.API.Tenants = map[string]config.APITenantConfig{}
		cfg.Langfuse.Enabled = false
	}))
	identityResponse := func(req *http.Request) (int, string) {
		if req.Header.Get("Authorization") != "Bearer dynamic-tenant-fixture" {
			return http.StatusOK, `{"data":{"active":false}}`
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"dynamic-tenant","aud":"ai-agent","exp":` + expirationAfter(time.Hour) + `}}`
	}
	installTestIntrospector(t, introspection, nil, identityResponse)
	t.Cleanup(func() {
		introspectionVerifierState.Lock()
		introspectionVerifierState.verifier = nil
		introspectionVerifierState.Unlock()
	})
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	login := httptest.NewRequest(http.MethodPost, "http://example.test/console/session", strings.NewReader(`{"token":"dynamic-tenant-fixture"}`))
	login.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, login)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatal("dynamic tenant could not establish a console session")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing console session cookie")
	}
	request := func(path, token, csrf, origin string, cookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader(`{"tenant_id":"other-tenant"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie {
			req.AddCookie(cookies[0])
		}
		req.Header.Set("X-Console-CSRF", csrf)
		req.Header.Set("Origin", origin)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	for _, tc := range []struct {
		name, path, token, csrf, origin string
		cookie                          bool
		want                            int
	}{
		{"bearer", "/api/tenant/prompt/init", "dynamic-tenant-fixture", "", "", false, 503},
		{"console", "/api/tenant/prompt/init", "", session.CSRF, "http://example.test", true, 503},
		{"missing csrf", "/api/tenant/prompt/init", "", "", "", true, 403},
		{"wrong csrf", "/api/tenant/prompt/init", "", "wrong", "", true, 403},
		{"foreign origin", "/api/tenant/prompt/init", "", session.CSRF, "https://foreign.invalid", true, 403},
		{"invalid bearer", "/api/tenant/prompt/init", "invalid", "", "", false, 401},
		{"missing credential", "/api/tenant/prompt/init", "", "", "", false, 401},
		{"system admin required", "/api/prompt/init", "dynamic-tenant-fixture", "", "", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := request(tc.path, tc.token, tc.csrf, tc.origin, tc.cookie)
			if out.Code != tc.want {
				t.Fatalf("status=%d want=%d", out.Code, tc.want)
			}
			if tc.want == 503 && !strings.Contains(out.Body.String(), "Langfuse prompt management is disabled") {
				t.Fatal("dynamic tenant did not reach tenant initialization")
			}
		})
	}
	// Requiring pre-registration remains an authentication policy. The new
	// endpoint must not bypass that policy even with a previously valid cookie.
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.API.Auth.Introspection.RequireKnownTenant = true }))
	introspection.RequireKnownTenant = true
	installTestIntrospector(t, introspection, nil, identityResponse)
	for _, cookie := range []bool{false, true} {
		token := "dynamic-tenant-fixture"
		if cookie {
			token = ""
		}
		if out := request("/api/tenant/prompt/init", token, session.CSRF, "", cookie); out.Code != 401 {
			t.Fatalf("require_known_tenant was bypassed: status=%d", out.Code)
		}
	}
}
