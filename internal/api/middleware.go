package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"

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
		expectedKey := cfg.API.APIKey
		if expectedKey == "" && len(cfg.API.Tenants) == 0 {
			if gin.Mode() == gin.TestMode {
				principal := Principal{TenantID: "default", Admin: true}
				c.Set(principalKey, principal)
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalContextKey{}, principal))
				c.Next()
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "API key is unset; authentication disabled. Failing closed."})
			c.Abort()
			return
		}

		// Check X-API-Key header
		clientKey := c.GetHeader("X-API-Key")
		if clientKey == "" {
			// Fallback to Authorization Bearer header
			authHeader := c.GetHeader("Authorization")
			const bearerPrefix = "Bearer "
			if len(authHeader) > len(bearerPrefix) && authHeader[:len(bearerPrefix)] == bearerPrefix {
				clientKey = authHeader[len(bearerPrefix):]
			}
		}

		// Use constant-time comparison to prevent timing attacks.
		// subtle.ConstantTimeCompare requires equal-length slices; the length
		// check is also done in constant time via subtle.ConstantTimeEq so that
		// response latency does not leak key length information.
		principal := Principal{}
		matched := false
		if constantTimeKeyMatch(clientKey, expectedKey) {
			principal = Principal{TenantID: "default", Admin: true}
			matched = true
		}
		for tenantID, tenant := range cfg.API.Tenants {
			if constantTimeKeyMatch(clientKey, tenant.APIKey) {
				principal = Principal{TenantID: tenantID, Admin: tenant.Admin}
				matched = true
			}
		}
		if !matched {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or missing API key"})
			c.Abort()
			return
		}

		c.Set(principalKey, principal)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalContextKey{}, principal))
		c.Next()
	}
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
