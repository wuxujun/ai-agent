package api

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func ErrorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) > 0 {
			err := c.Errors.Last()
			log.Printf("[API Error] Request: %s %s - Error: %v", c.Request.Method, c.Request.URL.Path, err.Err)
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
