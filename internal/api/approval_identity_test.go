package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func expectedIntrospectionActor(tenant, subject string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + subject))
	return fmt.Sprintf("introspection-sub:%x", digest[:12])
}

func TestIntrospectionPersonalActorSurvivesCacheAndTokenRefresh(t *testing.T) {
	cfg := testIntrospectionConfig()
	cfg.SubjectClaim = "data.sub"
	var requests atomic.Int32
	installTestIntrospector(t, cfg, &requests, func(req *http.Request) (int, string) {
		tenant := "project_a"
		if req.Header.Get("Authorization") == "Bearer other-tenant" {
			tenant = "project_b"
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"` + tenant + `","sub":"person-1","admin":true,"roles":["admin"],"aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}}`
	})
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
		c.API.Auth.Bearer.RequireSubject = true
		c.API.Tenants = map[string]config.APITenantConfig{"project_a": {}, "project_b": {}}
	}))
	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) {
		p := principalFromGin(c)
		if p.Admin {
			t.Error("upstream admin claim granted privileges")
		}
		c.String(http.StatusOK, p.ActorID)
	})
	for _, token := range []string{"opaque-1", "opaque-1", "opaque-refreshed"} {
		response := performJWTRequest(router, token)
		if response.Code != http.StatusOK || response.Body.String() != expectedIntrospectionActor("project_a", "person-1") {
			t.Fatalf("personal actor = %d %s", response.Code, response.Body.String())
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, expected one cached and one refreshed token", requests.Load())
	}
	other := performJWTRequest(router, "other-tenant")
	if other.Code != http.StatusOK || other.Body.String() != expectedIntrospectionActor("project_b", "person-1") || other.Body.String() == expectedIntrospectionActor("project_a", "person-1") {
		t.Fatalf("personal identity is not tenant scoped: %d %s", other.Code, other.Body.String())
	}
}

func TestIntrospectionConfiguredSubjectFailsClosed(t *testing.T) {
	for _, subject := range []string{"null", "1", `""`, `" spaced "`, `"bad\nsubject"`, `"` + strings.Repeat("x", 257) + `"`} {
		t.Run(fmt.Sprintf("subject-%x", sha256.Sum256([]byte(subject))), func(t *testing.T) {
			cfg := testIntrospectionConfig()
			cfg.SubjectClaim = "data.sub"
			installTestIntrospector(t, cfg, nil, func(*http.Request) (int, string) {
				return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"project_a","sub":` + subject + `,"aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}}`
			})
			t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
				c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
				c.API.Tenants = map[string]config.APITenantConfig{"project_a": {}}
			}))
			router := gin.New()
			router.Use(AuthMiddleware())
			router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
			if response := performJWTRequest(router, "opaque-token"); response.Code != http.StatusUnauthorized {
				t.Fatalf("invalid subject accepted: %d", response.Code)
			}
		})
	}
}

func TestJWTRequiredSubjectRejectsMissingPersonalIdentity(t *testing.T) {
	privateKey := mustRSAKey(t)
	jwtConfig := testJWTConfig()
	installTestJWTVerifier(jwtConfig, privateKey.PublicKey, nil)
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.Auth = config.APIAuthConfig{Mode: "jwt", JWT: jwtConfig}
		c.API.Auth.Bearer.RequireSubject = true
		c.API.Tenants = map[string]config.APITenantConfig{"project_a": {}}
	}))
	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, tc := range []struct {
		sub    any
		status int
	}{{nil, http.StatusUnauthorized}, {123, http.StatusUnauthorized}, {"person-1", http.StatusOK}} {
		claims := jwt.MapClaims{"iss": jwtConfig.Issuer, "aud": jwtConfig.Audience, "exp": time.Now().Add(time.Minute).Unix(), "code": "project_a"}
		if tc.sub != nil {
			claims["sub"] = tc.sub
		}
		token := signJWT(t, privateKey, claims)
		if response := performJWTRequest(router, token); response.Code != tc.status {
			t.Fatalf("subject=%v response=%d, want %d", tc.sub, response.Code, tc.status)
		}
	}
}

func TestApprovalPolicyRejectsBeforeResolution(t *testing.T) {
	for _, mode := range []string{"deny", "actors"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testIntrospectionConfig()
			cfg.SubjectClaim = "data.sub"
			installTestIntrospector(t, cfg, nil, func(*http.Request) (int, string) {
				return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"project_a","sub":"unlisted-person","roles":["approver"],"aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}}`
			})
			t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
				c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
				c.API.Tenants = map[string]config.APITenantConfig{"project_a": {ApprovalMode: mode, ApprovalActorIDs: []string{expectedIntrospectionActor("project_a", "approved-person")}}}
			}))
			st := store.NewMemoryStore()
			if err := st.CreateTask(t.Context(), &types.Task{ID: "policy-task", TenantID: "project_a", Status: types.StatusAwaitingApproval}); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			RegisterRoutes(router, st, nil, nil)
			for _, action := range []string{"approve", "reject"} {
				req := httptest.NewRequest(http.MethodPost, "/api/tasks/policy-task/"+action, strings.NewReader(`{"actor_id":"`+expectedIntrospectionActor("project_a", "approved-person")+`"}`))
				req.Header.Set("Authorization", "Bearer opaque-token")
				req.Header.Set("X-Actor-ID", expectedIntrospectionActor("project_a", "approved-person"))
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				if response.Code != http.StatusForbidden {
					t.Fatalf("%s denied actor reached resolver: %d", action, response.Code)
				}
			}
			req := httptest.NewRequest(http.MethodGet, "/api/tasks/policy-task/approvals", nil)
			req.Header.Set("Authorization", "Bearer opaque-token")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("approval read was incorrectly forbidden: %d", response.Code)
			}
		})
	}
}

func TestApprovalPolicyAllowlistAndLiveRevocation(t *testing.T) {
	cfg := testIntrospectionConfig()
	cfg.SubjectClaim = "data.sub"
	installTestIntrospector(t, cfg, nil, func(*http.Request) (int, string) {
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"project_a","sub":"approved-person","aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}}`
	})
	restore := config.OverrideForTesting(func(c *config.Config) {
		c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
		c.API.Tenants = map[string]config.APITenantConfig{"project_a": {ApprovalMode: "actors", ApprovalActorIDs: []string{expectedIntrospectionActor("project_a", "approved-person")}}}
	})
	t.Cleanup(restore)
	router := gin.New()
	router.Use(AuthMiddleware())
	router.POST("/", ApprovalAuthorizationMiddleware(), func(c *gin.Context) { c.String(http.StatusOK, principalFromGin(c).ActorID) })
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer opaque-token")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if response := request(); response.Code != http.StatusOK || response.Body.String() != expectedIntrospectionActor("project_a", "approved-person") {
		t.Fatalf("allowlisted actor denied: %d %s", response.Code, response.Body.String())
	}
	restore()
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
		c.API.Tenants = map[string]config.APITenantConfig{"project_a": {ApprovalMode: "deny"}}
	}))
	if response := request(); response.Code != http.StatusForbidden {
		t.Fatalf("cached identity bypassed policy revocation: %d", response.Code)
	}
}
