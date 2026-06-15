package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

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
		if expectedKey == "" {
			c.Next()
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

		if clientKey != expectedKey {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: invalid or missing API key"})
			c.Abort()
			return
		}

		c.Next()
	}
}
