package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
)

func TestIntrospectionAuthMapsNestedUserIDAndCachesByTokenHash(t *testing.T) {
	gin.SetMode(gin.TestMode)
	introspectionConfig := testIntrospectionConfig()
	var requests atomic.Int32
	installTestIntrospector(t, introspectionConfig, &requests, func(req *http.Request) (int, string) {
		if req.Method != http.MethodGet {
			t.Fatalf("unexpected introspection method: %s", req.Method)
		}
		if authorization := req.Header.Get("Authorization"); authorization != "Bearer opaque-access-token" {
			t.Fatalf("unexpected introspection authorization header")
		}
		if req.Body != nil {
			t.Fatalf("GET introspection request must not contain a body")
		}
		return http.StatusOK, `{"code":0,"success":true,"message":"ok","error":"","requestID":"request-id","data":{"active":true,"userid":"project_a"},"iss":"https://issuer.example","aud":["ai-agent"],"exp":` + expirationAfter(time.Minute) + `}`
	})
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "admin-static-key"
		cfg.API.Auth = config.APIAuthConfig{
			Mode:          "hybrid",
			Bearer:        config.APIBearerConfig{ValidationMode: "introspection"},
			Introspection: introspectionConfig,
		}
		cfg.API.Tenants = map[string]config.APITenantConfig{"project_a": {Admin: false}}
	}))

	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) {
		principal := principalFromGin(c)
		c.JSON(http.StatusOK, gin.H{"tenant_id": principal.TenantID, "admin": principal.Admin})
	})

	for i := 0; i < 2; i++ {
		response := performJWTRequest(router, "opaque-access-token")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tenant_id":"project_a"`) {
			t.Fatalf("introspection response = %d %s", response.Code, response.Body.String())
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("introspection requests = %d, want one cached request", requests.Load())
	}

	staticRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	staticRequest.Header.Set("X-API-Key", "admin-static-key")
	staticResponse := httptest.NewRecorder()
	router.ServeHTTP(staticResponse, staticRequest)
	if staticResponse.Code != http.StatusOK || !strings.Contains(staticResponse.Body.String(), `"admin":true`) {
		t.Fatalf("hybrid static key response = %d %s", staticResponse.Code, staticResponse.Body.String())
	}
}

func TestIntrospectionAuthRejectsInvalidResponsesAndFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	introspectionConfig := testIntrospectionConfig()
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: introspectionConfig}
		cfg.API.Tenants = map[string]config.APITenantConfig{"project_a": {}}
	}))
	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "inactive", status: 200, body: `{"code":0,"success":true,"data":{"active":false,"userid":"project_a"}}`, wantStatus: http.StatusUnauthorized},
		{name: "missing active", status: 200, body: `{"code":0,"success":true,"data":{"userid":"project_a"}}`, wantStatus: http.StatusUnauthorized},
		{name: "unknown tenant", status: 200, body: `{"code":0,"success":true,"data":{"active":true,"userid":"project_b"},"iss":"https://issuer.example","aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}`, wantStatus: http.StatusUnauthorized},
		{name: "expired", status: 200, body: `{"code":0,"success":true,"data":{"active":true,"userid":"project_a"},"iss":"https://issuer.example","aud":"ai-agent","exp":` + expirationAfter(-time.Minute) + `}`, wantStatus: http.StatusUnauthorized},
		{name: "wrong issuer", status: 200, body: `{"code":0,"success":true,"data":{"active":true,"userid":"project_a"},"iss":"https://attacker.example","aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}`, wantStatus: http.StatusUnauthorized},
		{name: "malformed response", status: 200, body: `{`, wantStatus: http.StatusServiceUnavailable},
		{name: "oversized response", status: 200, body: `{"active":true,"padding":"` + strings.Repeat("x", maxIntrospectionResponseBytes) + `"}`, wantStatus: http.StatusServiceUnavailable},
		{name: "upstream failure", status: 500, body: `failure`, wantStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installTestIntrospector(t, introspectionConfig, nil, func(*http.Request) (int, string) {
				return tt.status, tt.body
			})
			response := performJWTRequest(router, "opaque-token-"+tt.name)
			if response.Code != tt.wantStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), tt.wantStatus)
			}
		})
	}
}

func testIntrospectionConfig() config.APIIntrospectionConfig {
	return config.APIIntrospectionConfig{
		URL:                "https://auth.example/oauth/introspect",
		TenantClaim:        "data.userid",
		ActiveClaim:        "data.active",
		Issuer:             "https://issuer.example",
		Audience:           "ai-agent",
		RequireExpiration:  true,
		RequireKnownTenant: true,
		TimeoutSeconds:     3,
		CacheTTLSeconds:    10,
	}
}

func installTestIntrospector(t *testing.T, introspectionConfig config.APIIntrospectionConfig, requests *atomic.Int32, response func(*http.Request) (int, string)) {
	t.Helper()
	client := &http.Client{Transport: jwtRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if requests != nil {
			requests.Add(1)
		}
		status, body := response(req)
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	introspectionVerifierState.Lock()
	introspectionVerifierState.config = introspectionConfig
	introspectionVerifierState.verifier = &introspectionVerifier{
		config: introspectionConfig,
		client: client,
		cache:  make(map[[32]byte]cachedIntrospection),
	}
	introspectionVerifierState.Unlock()
}

func expirationAfter(duration time.Duration) string {
	value, _ := json.Marshal(time.Now().Add(duration).Unix())
	return string(value)
}
