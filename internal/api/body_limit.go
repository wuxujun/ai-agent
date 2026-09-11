package api

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes = 10 << 20

// Check the entire bounded body before any handler can act on a partially
// decoded approval or session request. http.Server bounds header/body read time;
// the buffered body allows long-lived SSE responses without a write deadline.
func RequestBodyLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxRequestBodyBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body exceeds 10 MiB limit"})
			return
		}
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(limited)
		_ = limited.Close()
		if err != nil {
			var oversized *http.MaxBytesError
			var timeout net.Error
			status, message := http.StatusBadRequest, "failed to read request body"
			if errors.As(err, &oversized) {
				status, message = http.StatusRequestEntityTooLarge, "request body exceeds 10 MiB limit"
			} else if errors.As(err, &timeout) && timeout.Timeout() {
				status, message = http.StatusRequestTimeout, "request body read timed out"
			}
			c.AbortWithStatusJSON(status, gin.H{"error": message})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Next()
	}
}
