package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSessionLifecycleAndTaskAssociation(t *testing.T) {
	st := store.NewMemoryStore()
	router := setupTestRouter(t, st, nil)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewBufferString(`{"id":"session-a","title":"Research"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create session status=%d body=%s", response.Code, response.Body.String())
	}

	for _, id := range []string{"task-a", "task-b"} {
		body, _ := json.Marshal(map[string]any{"id": id, "session_id": "session-a", "goal": id, "workspace": "./testdata"})
		response = httptest.NewRecorder()
		request = httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create task %s status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	first, _ := st.GetTask(request.Context(), "task-a")
	second, _ := st.GetTask(request.Context(), "task-b")
	if first.SessionID != "session-a" || first.SequenceNo != 1 || second.SequenceNo != 2 {
		t.Fatalf("unexpected task session sequence: first=%+v second=%+v", first, second)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/session-a/tasks", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"count":2`)) {
		t.Fatalf("list session tasks status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/session-a/tasks?limit=1&offset=1", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"count":1`)) || !bytes.Contains(response.Body.Bytes(), []byte(`"id":"task-b"`)) {
		t.Fatalf("paginated session tasks status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPatch, "/api/sessions/session-a", bytes.NewBufferString(`{"title":" "}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty session title accepted: status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions/session-a/archive", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", response.Code, response.Body.String())
	}

	body, _ := json.Marshal(map[string]any{"id": "task-c", "session_id": "session-a", "goal": "blocked", "workspace": "./testdata"})
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("archived session accepted task: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSessionTenantIsolation(t *testing.T) {
	st := store.NewMemoryStore()
	if err := st.CreateSession(t.Context(), &types.Session{ID: "shared-id", TenantID: "tenant-a", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(t.Context(), "shared-id", "tenant-b"); err != store.ErrSessionNotFound {
		t.Fatalf("cross-tenant session lookup err=%v", err)
	}
	if _, err := st.NextSessionTaskSequence(t.Context(), "shared-id", "tenant-b"); err != store.ErrSessionNotFound {
		t.Fatalf("cross-tenant sequence allocation err=%v", err)
	}
}
