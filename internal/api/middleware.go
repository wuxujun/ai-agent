package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type Principal struct {
	TenantID string
	Admin    bool
}

const principalKey = "authenticated_principal"

type principalContextKey struct{}

func principalFromGin(c *gin.Context) Principal {
	principal, _ := c.MustGet(principalKey).(Principal)
	return principal
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

func ErrorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) > 0 {
			err := c.Errors.Last()
			log.Error("request error",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"error", err.Err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
			})
		}
	}
}

func SpanAttributesMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		span := trace.SpanFromContext(c.Request.Context())
		if taskID := c.Param("id"); taskID != "" {
			span.SetAttributes(attribute.String("agent.task.id", taskID))
		}
		c.Next()
	}
}

func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := config.Get()
		authMode := strings.ToLower(strings.TrimSpace(cfg.API.Auth.Mode))
		if authMode == "" {
			authMode = "api_key"
		}
		apiKeyEnabled := authMode == "api_key" || authMode == "hybrid"
		bearerEnabled := authMode == "jwt" || authMode == "introspection" || authMode == "hybrid"
		xAPIKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
		bearerToken := bearerCredential(c.GetHeader("Authorization"))
		if gin.Mode() == gin.TestMode && xAPIKey == "" && bearerToken == "" && !hasConfiguredAPIKey(cfg) {
			principal := Principal{TenantID: "default", Admin: true}
			setPrincipal(c, principal)
			c.Next()
			return
		}
		if apiKeyEnabled && !bearerEnabled && !hasConfiguredAPIKey(cfg) {
			if gin.Mode() == gin.TestMode {
				principal := Principal{TenantID: "default", Admin: true}
				setPrincipal(c, principal)
				c.Next()
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "API key is unset; authentication disabled. Failing closed."})
			c.Abort()
			return
		}

		// X-API-Key is always a local credential. Never forward it to JWT or
		// introspection providers, even when external Bearer auth is enabled.
		if xAPIKey != "" {
			if principal, matched := matchStaticAPIKey(cfg, xAPIKey); matched {
				setPrincipal(c, principal)
				c.Next()
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or missing credential"})
			c.Abort()
			return
		}
		if apiKeyEnabled && bearerToken != "" {
			if principal, matched := matchStaticAPIKey(cfg, bearerToken); matched {
				setPrincipal(c, principal)
				c.Next()
				return
			}
		}
		if bearerEnabled && bearerToken != "" {
			tenantID, requireKnownTenant, err := verifyBearerCredential(c.Request.Context(), cfg, authMode, bearerToken)
			if err == nil {
				tenant, known := cfg.API.Tenants[tenantID]
				if known || !requireKnownTenant {
					setPrincipal(c, Principal{TenantID: tenantID, Admin: known && tenant.Admin})
					c.Next()
					return
				}
			}
			if errors.Is(err, errAuthenticationUnavailable) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication service unavailable"})
				c.Abort()
				return
			}
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or missing credential"})
		c.Abort()
	}
}

func verifyBearerCredential(ctx context.Context, cfg *config.Config, authMode, bearerToken string) (string, bool, error) {
	validationMode := authMode
	if authMode == "hybrid" {
		validationMode = strings.ToLower(strings.TrimSpace(cfg.API.Auth.Bearer.ValidationMode))
		if validationMode == "" {
			validationMode = "jwks"
		}
	}
	switch validationMode {
	case "jwt", "jwks":
		tenantID, err := verifierFor(cfg.API.Auth.JWT).verify(ctx, bearerToken)
		return tenantID, cfg.API.Auth.JWT.RequireKnownTenant, err
	case "introspection":
		tenantID, err := introspectorFor(cfg.API.Auth.Introspection).verify(ctx, bearerToken)
		return tenantID, cfg.API.Auth.Introspection.RequireKnownTenant, err
	default:
		return "", true, errInvalidCredential
	}
}

func hasConfiguredAPIKey(cfg *config.Config) bool {
	if strings.TrimSpace(cfg.API.APIKey) != "" {
		return true
	}
	for _, tenant := range cfg.API.Tenants {
		if strings.TrimSpace(tenant.APIKey) != "" {
			return true
		}
	}
	return false
}

func bearerCredential(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func matchStaticAPIKey(cfg *config.Config, candidate string) (Principal, bool) {
	if constantTimeKeyMatch(candidate, cfg.API.APIKey) {
		return Principal{TenantID: "default", Admin: true}, true
	}
	for tenantID, tenant := range cfg.API.Tenants {
		if constantTimeKeyMatch(candidate, tenant.APIKey) {
			return Principal{TenantID: tenantID, Admin: tenant.Admin}, true
		}
	}
	return Principal{}, false
}

func setPrincipal(c *gin.Context, principal Principal) {
	c.Set(principalKey, principal)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalContextKey{}, principal))
}

func constantTimeKeyMatch(actual, expected string) bool {
	if expected == "" {
		return false
	}
	actualBytes, expectedBytes := []byte(actual), []byte(expected)
	return subtle.ConstantTimeEq(int32(len(actualBytes)), int32(len(expectedBytes))) == 1 && subtle.ConstantTimeCompare(actualBytes, expectedBytes) == 1
}

func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !principalFromGin(c).Admin {
			c.JSON(http.StatusForbidden, gin.H{"error": "administrator access required"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func TaskTenantMiddleware(st store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("id")
		principal := principalFromGin(c)
		if taskID == "" || principal.Admin {
			c.Next()
			return
		}
		task, err := st.GetTask(c.Request.Context(), taskID)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
				c.Abort()
				return
			}
			c.Error(err)
			c.Abort()
			return
		}
		if task.TenantID != principal.TenantID {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			c.Abort()
			return
		}
		c.Next()
	}
}
