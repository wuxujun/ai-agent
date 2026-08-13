package multiagenteval

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRAGStubHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"query":"release"}`))
	recorder := httptest.NewRecorder()
	RAGStubHandler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), RAGStubToken) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
