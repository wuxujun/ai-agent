package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	projectlogger "github.com/wuxujun/ai-agent/internal/logger"
)

func TestAccessLogMiddlewareWritesStructuredRecordsWithoutSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	directory := t.TempDir()
	if err := projectlogger.Configure(projectlogger.Options{
		Level:         "info",
		Console:       false,
		FileEnabled:   true,
		AccessEnabled: true,
		Directory:     directory,
		RetentionDays: 7,
	}); err != nil {
		t.Fatalf("configure logger: %v", err)
	}
	t.Cleanup(func() {
		_ = projectlogger.Close()
		projectlogger.Reinit("info")
	})

	router := gin.New()
	router.Use(AccessLogMiddleware())
	router.Use(RecoveryMiddleware())
	router.Use(ErrorMiddleware())
	router.Use(func(c *gin.Context) {
		setPrincipal(c, Principal{TenantID: "tenant-6492"})
		c.Next()
	})
	router.GET("/api/tasks/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.GET("/failure", func(c *gin.Context) {
		_ = c.Error(errors.New("expected access test failure"))
	})
	router.GET("/panic", func(*gin.Context) {
		panic("expected access test panic")
	})

	request := httptest.NewRequest(http.MethodGet, "/api/tasks/task-42?token=query-secret", nil)
	request.Header.Set("X-Request-ID", "request-123")
	request.Header.Set("Authorization", "Bearer authorization-secret")
	request.Header.Set("X-API-Key", "api-key-secret")
	request.Header.Set("Cookie", "session=cookie-secret")
	request.Header.Set("User-Agent", "access-test-agent")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("route response = %d", response.Code)
	}
	if response.Header().Get("X-Request-ID") != "request-123" {
		t.Fatalf("response request ID = %q", response.Header().Get("X-Request-ID"))
	}

	missingRequest := httptest.NewRequest(http.MethodPost, "/missing?password=query-password", strings.NewReader("body-secret"))
	missingRequest.Header.Set("X-Request-ID", "invalid request id")
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingRequest)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing route response = %d", missingResponse.Code)
	}
	if generated := missingResponse.Header().Get("X-Request-ID"); generated == "" || generated == "invalid request id" {
		t.Fatalf("invalid request ID was not replaced: %q", generated)
	}

	failureRequest := httptest.NewRequest(http.MethodGet, "/failure", nil)
	failureResponse := httptest.NewRecorder()
	router.ServeHTTP(failureResponse, failureRequest)
	if failureResponse.Code != http.StatusInternalServerError {
		t.Fatalf("failure route response = %d %s", failureResponse.Code, failureResponse.Body.String())
	}

	panicRequest := httptest.NewRequest(http.MethodGet, "/panic", nil)
	panicResponse := httptest.NewRecorder()
	router.ServeHTTP(panicResponse, panicRequest)
	if panicResponse.Code != http.StatusInternalServerError {
		t.Fatalf("panic route response = %d %s", panicResponse.Code, panicResponse.Body.String())
	}

	if err := projectlogger.Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}
	path := filepath.Join(directory, "access-"+time.Now().Format(time.DateOnly)+".log")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read access log: %v", err)
	}
	text := string(content)
	for _, secret := range []string{"query-secret", "query-password", "authorization-secret", "api-key-secret", "cookie-secret", "body-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("access log leaked %q: %s", secret, text)
		}
	}

	var records []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode access record: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("access record count = %d, want 4: %s", len(records), text)
	}
	first := records[0]
	if first["app_version"] == nil || first["component"] != "access" || first["method"] != http.MethodGet || first["path"] != "/api/tasks/task-42" || first["route"] != "/api/tasks/:id" || first["task_id"] != "task-42" || first["tenant_id"] != "tenant-6492" || first["request_id"] != "request-123" || first["user_agent"] != "access-test-agent" {
		t.Fatalf("unexpected first access record: %#v", first)
	}
	if status, ok := first["status"].(float64); !ok || int(status) != http.StatusOK {
		t.Fatalf("first access status = %#v", first["status"])
	}
	second := records[1]
	if second["path"] != "/missing" || second["route"] != nil {
		t.Fatalf("unexpected 404 access record: %#v", second)
	}
	if status, ok := second["status"].(float64); !ok || int(status) != http.StatusNotFound {
		t.Fatalf("second access status = %#v", second["status"])
	}
	third := records[2]
	if third["path"] != "/failure" {
		t.Fatalf("unexpected failure access record: %#v", third)
	}
	if status, ok := third["status"].(float64); !ok || int(status) != http.StatusInternalServerError {
		t.Fatalf("failure access status = %#v", third["status"])
	}
	fourth := records[3]
	if fourth["path"] != "/panic" {
		t.Fatalf("unexpected panic access record: %#v", fourth)
	}
	if status, ok := fourth["status"].(float64); !ok || int(status) != http.StatusInternalServerError {
		t.Fatalf("panic access status = %#v", fourth["status"])
	}
}
