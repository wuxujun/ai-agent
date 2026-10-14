package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func installSelfApprovalIdentity(t *testing.T, enabled bool) {
	t.Helper()
	cfg := testIntrospectionConfig()
	cfg.SubjectClaim = "data.sub"
	installTestIntrospector(t, cfg, nil, func(req *http.Request) (int, string) {
		subject := "creator"
		tenant := "project_a"
		switch req.Header.Get("Authorization") {
		case "Bearer other":
			subject = "reviewer"
		case "Bearer admin":
			subject, tenant = "admin", "project_b"
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"` + tenant + `","sub":"` + subject + `","aud":"ai-agent","exp":` + expirationAfter(time.Minute) + `}}`
	})
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = ""
		c.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: cfg}
		c.API.Tenants = map[string]config.APITenantConfig{
			"project_a": {ApprovalDisallowSelf: enabled},
			"project_b": {Admin: true},
		}
	}))
}

func selfApprovalRequest(router http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor-ID", expectedIntrospectionActor("project_a", "reviewer"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func TestSelfApprovalCreatorCapturedFromAPIAndConsole(t *testing.T) {
	installSelfApprovalIdentity(t, true)
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x68}, 32)))
	path := filepath.Join(t.TempDir(), "tasks.db")
	st, err := store.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	workspace := "./testdata"
	for _, console := range []bool{false, true} {
		id := "api-creator"
		if console {
			id = "console-creator"
		}
		body, _ := json.Marshal(map[string]any{"id": id, "goal": "fixture", "workspace": workspace, "creator_actor_id": expectedIntrospectionActor("project_a", "reviewer"), "actor_id": "forged"})
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Actor-ID", expectedIntrospectionActor("project_a", "reviewer"))
		if console {
			cookie, err := encryptConsoleSession(consoleSession{Token: "creator-token", TenantID: "project_a", CSRFToken: "csrf", ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: consoleCookieName, Value: cookie})
			req.Header.Set("X-Console-CSRF", "csrf")
		} else {
			req.Header.Set("Authorization", "Bearer creator-token")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusCreated {
			t.Fatalf("console=%v create: %d %s", console, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"creator_actor_id":"`+expectedIntrospectionActor("project_a", "creator")+`"`) {
			t.Fatalf("creator missing in response: %s", response.Body.String())
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for _, id := range []string{"api-creator", "console-creator"} {
		task, err := reopened.GetTaskWithoutTrace(t.Context(), id)
		if err != nil || task.CreatorActorID != expectedIntrospectionActor("project_a", "creator") {
			t.Fatalf("reopened creator: %+v %v", task, err)
		}
	}
}

func TestSelfApprovalDeniesBeforeLocalAndDurableResolution(t *testing.T) {
	for _, action := range []string{"approve", "reject"} {
		for _, explicit := range []bool{false, true} {
			for _, creator := range []string{expectedIntrospectionActor("project_a", "creator"), "", "tenant:project_a", "introspection-sub:invalid"} {
				t.Run(action+"/"+creator+"/"+map[bool]string{true: "explicit", false: "implicit"}[explicit], func(t *testing.T) {
					installSelfApprovalIdentity(t, true)
					st := store.NewMemoryStore()
					task := &types.Task{ID: "self-task", TenantID: "project_a", CreatorActorID: creator, Status: types.StatusAwaitingApproval}
					if err := st.CreateTask(t.Context(), task); err != nil {
						t.Fatal(err)
					}
					id, ch := orchestrator.RegisterApproval(task.ID, &types.ApprovalRequest{TaskID: task.ID, Action: "write_file"})
					t.Cleanup(func() { orchestrator.RemoveApproval(id) })
					if err := st.CreateApproval(t.Context(), &types.DurableApproval{ID: id, TaskID: task.ID, TenantID: task.TenantID, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: id, TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("encrypted-fixture")}); err != nil {
						t.Fatal(err)
					}
					engine := &orchestrator.Engine{Store: st}
					router := gin.New()
					RegisterRoutes(router, st, engine, nil)
					body := `{"actor_id":"` + expectedIntrospectionActor("project_a", "reviewer") + `"}`
					if explicit {
						body = `{"approval_id":"` + id + `","actor_id":"forged"}`
					}
					for _, token := range []string{"creator-token", "refreshed-token"} {
						response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/self-task/"+action, token, body)
						if response.Code != http.StatusForbidden {
							t.Fatalf("decision reached resolver: %d %s", response.Code, response.Body.String())
						}
					}
					durable, err := st.GetApproval(t.Context(), id, task.TenantID)
					if err != nil || durable.Status != types.ApprovalPending || len(durable.ResolutionPayload) != 0 {
						t.Fatalf("durable changed: %+v %v", durable, err)
					}
					select {
					case <-ch:
						t.Fatal("local approval channel signaled")
					default:
					}
					if response := selfApprovalRequest(router, http.MethodGet, "/api/tasks/self-task/approvals", "creator-token", ""); response.Code != http.StatusOK {
						t.Fatalf("read denied: %d", response.Code)
					}
				})
			}
		}
	}
}

func TestSelfApprovalDifferentPersonAndAdmin(t *testing.T) {
	installSelfApprovalIdentity(t, true)
	for _, token := range []string{"other", "admin"} {
		for _, action := range []string{"approve", "reject"} {
			t.Run(token+"/"+action, func(t *testing.T) {
				st := store.NewMemoryStore()
				task := &types.Task{ID: "independent", TenantID: "project_a", CreatorActorID: expectedIntrospectionActor("project_a", "creator"), Status: types.StatusAwaitingApproval}
				if err := st.CreateTask(t.Context(), task); err != nil {
					t.Fatal(err)
				}
				id, ch := orchestrator.RegisterApproval(task.ID, &types.ApprovalRequest{TaskID: task.ID, Action: "write_file"})
				t.Cleanup(func() { orchestrator.RemoveApproval(id) })
				router := gin.New()
				RegisterRoutes(router, st, nil, nil)
				response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/independent/"+action, token, `{"approval_id":"`+id+`"}`)
				if response.Code != http.StatusOK {
					t.Fatalf("independent decision: %d %s", response.Code, response.Body.String())
				}
				select {
				case result := <-ch:
					if result.Approved != (action == "approve") {
						t.Fatal("wrong decision")
					}
				default:
					t.Fatal("decision missing")
				}
			})
		}
	}
	// A cross-tenant administrator remains subject to the task owner's policy.
	st := store.NewMemoryStore()
	if err := st.CreateTask(t.Context(), &types.Task{ID: "admin-self", TenantID: "project_a", CreatorActorID: expectedIntrospectionActor("project_b", "admin")}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	if response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/admin-self/approve", "admin", ""); response.Code != http.StatusForbidden {
		t.Fatalf("admin bypassed task policy: %d", response.Code)
	}
}

func TestSelfApprovalNonPersonalCreationAndDecisionFailClosed(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = ""
		c.API.Auth.Mode = "api_key"
		c.API.Tenants = map[string]config.APITenantConfig{"project_a": {APIKey: "fixture-key", ApprovalDisallowSelf: true}}
	}))
	st := store.NewMemoryStore()
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	workspace, _ := json.Marshal(t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"id":"no-person","goal":"fixture","workspace":`+string(workspace)+`}`))
	req.Header.Set("X-API-Key", "fixture-key")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("nonpersonal creation: %d %s", response.Code, response.Body.String())
	}
	if exists, _ := st.ExistsTask(t.Context(), "no-person"); exists {
		t.Fatal("rejected task persisted")
	}
	if err := st.CreateTask(t.Context(), &types.Task{ID: "existing", TenantID: "project_a", CreatorActorID: expectedIntrospectionActor("project_a", "creator")}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/existing/approve", nil)
	req.Header.Set("X-API-Key", "fixture-key")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("nonpersonal decision: %d", response.Code)
	}
}

func TestSelfApprovalPolicyAppliesToExistingTasksImmediately(t *testing.T) {
	installSelfApprovalIdentity(t, false)
	st := store.NewMemoryStore()
	if err := st.CreateTask(t.Context(), &types.Task{ID: "toggle", TenantID: "project_a"}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	if response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/toggle/approve", "creator-token", ""); response.Code != http.StatusNotFound {
		t.Fatalf("legacy behavior: %d", response.Code)
	}
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		tenant := c.API.Tenants["project_a"]
		tenant.ApprovalDisallowSelf = true
		c.API.Tenants["project_a"] = tenant
	}))
	if response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/toggle/approve", "creator-token", ""); response.Code != http.StatusForbidden {
		t.Fatalf("cached identity bypassed new policy: %d", response.Code)
	}
}
