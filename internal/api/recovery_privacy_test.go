package api

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRecoveryOmitsPrivateRequestAndPanicValues(t *testing.T) {
	oldMode, oldWriter, oldLog := gin.Mode(), gin.DefaultErrorWriter, log
	t.Cleanup(func() { gin.SetMode(oldMode); gin.DefaultErrorWriter = oldWriter; log = oldLog })
	for _, mode := range []string{gin.ReleaseMode, gin.DebugMode} {
		for _, kind := range []string{"ordinary", "epipe", "reset"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				var raw, structured bytes.Buffer
				gin.SetMode(mode)
				gin.DefaultErrorWriter = &raw
				log = slog.New(slog.NewJSONHandler(&structured, nil))
				var problem any = "PANIC_PRIVATE_MARKER"
				if kind != "ordinary" {
					errno := syscall.EPIPE
					if kind == "reset" {
						errno = syscall.ECONNRESET
					}
					problem = &net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: fmt.Errorf("PANIC_PRIVATE_MARKER: %w", errno)}}
				}
				router := gin.New()
				router.Use(RecoveryMiddleware())
				router.GET("/panic", func(*gin.Context) { panic(problem) })
				req := httptest.NewRequest(http.MethodGet, "/panic?token=QUERY_PRIVATE_MARKER", strings.NewReader("BODY_PRIVATE_MARKER"))
				req.Header.Set("X-API-Key", "API_PRIVATE_MARKER")
				req.Header.Set("Cookie", "COOKIE_PRIVATE_MARKER")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				for _, marker := range []string{"PANIC_PRIVATE_MARKER", "API_PRIVATE_MARKER", "COOKIE_PRIVATE_MARKER", "QUERY_PRIVATE_MARKER", "BODY_PRIVATE_MARKER"} {
					if strings.Contains(raw.String()+structured.String(), marker) {
						t.Errorf("recovery exposed %s", marker)
					}
				}
				if recorder.Code != 500 || !strings.Contains(structured.String(), "panic recovered") {
					t.Errorf("recovery status=%d missing safe diagnostic", recorder.Code)
				}
			})
		}
	}
}
