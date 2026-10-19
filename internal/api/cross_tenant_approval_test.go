package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Synthetic identity contract: userid is both the stable personal subject and
// the user-scoped tenant ID. Only server-side policy grants global decisions.
func installCrossTenantApprovalIdentity(t *testing.T, simulationURL string) {
	t.Helper()
	mode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(mode) })
	in := testIntrospectionConfig()
	in.SubjectClaim = in.TenantClaim
	if simulationURL == "" {
		installTestIntrospector(t, in, nil, func(req *http.Request) (int, string) {
			userid, active, exp := "7001", true, expirationAfter(time.Minute)
			switch req.Header.Get("Authorization") {
			case "Bearer reviewer", "Bearer reviewer-refreshed", "Bearer expired":
				userid = "6492"
			case "Bearer unlisted":
				userid = "7002"
			case "Bearer creator", "Bearer creator-refreshed":
			default:
				active = false
			}
			if req.Header.Get("Authorization") == "Bearer expired" {
				exp = expirationAfter(-time.Minute)
			}
			body := map[string]any{"iss": in.Issuer, "data": map[string]any{"userid": userid, "active": active, "aud": in.Audience, "exp": json.Number(exp), "admin": true, "roles": []string{"approver", "admin"}}}
			b, _ := json.Marshal(body)
			return http.StatusOK, string(b)
		})
	} else {
		// Explicit opt-in for local HTTP simulation; never alter production URL
		// validation or public-network restrictions to run this fixture.
		parsed, err := url.Parse(simulationURL)
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatal("simulation must use an explicit loopback HTTP endpoint")
		}
		in.URL = simulationURL
		introspectionVerifierState.Lock()
		introspectionVerifierState.config = in
		introspectionVerifierState.verifier = &introspectionVerifier{config: in, client: policy.ConfiguredHTTPClient(3*time.Second, true), cache: make(map[[32]byte]cachedIntrospection)}
		introspectionVerifierState.Unlock()
	}
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = ""
		c.API.Auth = config.APIAuthConfig{Mode: "introspection", Bearer: config.APIBearerConfig{RequireSubject: true}, Introspection: in}
		c.API.Tenants = map[string]config.APITenantConfig{
			"6492": {ApprovalMode: "actors", ApprovalActorIDs: []string{expectedIntrospectionActor("6492", "6492")}, ApprovalDisallowSelf: true, ApprovalCrossTenant: true, APIKey: "local-reviewer-key"},
			"7001": {}, "7002": {},
		}
	}))
}

func crossTenantApprovalFixture(t *testing.T, st store.TaskCreationStore, id, tenant, creator string) (string, <-chan types.ApprovalResult) {
	t.Helper()
	if err := st.CreateTask(t.Context(), &types.Task{ID: id, TenantID: tenant, CreatorActorID: creator, Status: types.StatusAwaitingApproval}); err != nil {
		t.Fatal(err)
	}
	aid, ch := orchestrator.RegisterApproval(id, &types.ApprovalRequest{TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh})
	t.Cleanup(func() { orchestrator.RemoveApproval(aid) })
	return aid, ch
}

func crossTenantApprovalCodec(t *testing.T) *approvalcrypto.Codec {
	t.Helper()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x59}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func runCrossTenantApprovalDecisions(t *testing.T) {
	t.Helper()
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	router := gin.New()
	codec := crossTenantApprovalCodec(t)
	RegisterRoutes(router, st, &orchestrator.Engine{Store: st, ApprovalCodec: codec}, nil)
	for _, action := range []string{"approve", "reject"} {
		for _, explicit := range []bool{false, true} {
			id := action + map[bool]string{true: "-explicit", false: "-implicit"}[explicit]
			// Exercise API creator capture under the shared userid field contract.
			created := selfApprovalRequest(router, http.MethodPost, "/api/tasks", "creator", `{"id":"`+id+`","goal":"local acceptance fixture only","workspace":"./testdata","creator_actor_id":"forged"}`)
			if created.Code != http.StatusCreated {
				t.Fatalf("fixture creation: %d", created.Code)
			}
			task, err := st.GetTask(t.Context(), id)
			if err != nil || task.CreatorActorID != expectedIntrospectionActor("7001", "7001") {
				t.Fatal("verified creator was not captured")
			}
			aid, ch := orchestrator.RegisterApproval(id, &types.ApprovalRequest{TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh})
			t.Cleanup(func() { orchestrator.RemoveApproval(aid) })
			if err := st.CreateApproval(t.Context(), &types.DurableApproval{ID: aid, TaskID: id, TenantID: "7001", Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: aid, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("opaque-fixture")}); err != nil {
				t.Fatal(err)
			}
			body := `{"actor_id":"forged"}`
			if explicit {
				body = `{"approval_id":"` + aid + `","actor_id":"forged"}`
			}
			for _, attempt := range []struct {
				token string
				want  int
			}{{"unlisted", 404}, {"invalid", 401}, {"expired", 401}, {"", 401}} {
				response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/"+id+"/"+action, attempt.token, body)
				if response.Code != attempt.want {
					t.Fatalf("%s unauthorized decision: %d, want %d", action, response.Code, attempt.want)
				}
			}
			select {
			case <-ch:
				t.Fatal("forbidden decision signaled approval")
			default:
			}
			record, err := st.GetApproval(t.Context(), aid, "7001")
			if err != nil || record.Status != types.ApprovalPending || len(record.ResolutionPayload) != 0 {
				t.Fatal("forbidden decision changed durable approval")
			}
			response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/"+id+"/"+action, "reviewer", body)
			if response.Code != http.StatusOK {
				t.Fatalf("authorized %s: %d", action, response.Code)
			}
			select {
			case result := <-ch:
				if result.ActorID != expectedIntrospectionActor("6492", "6492") || result.Approved != (action == "approve") {
					t.Fatal("decision audit identity/result mismatch")
				}
			default:
				t.Fatal("authorized decision missing")
			}
			record, err = st.GetApproval(t.Context(), aid, "7001")
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := types.ApprovalRejected
			if action == "approve" {
				wantStatus = types.ApprovalApproved
			}
			plain, err := codec.Decrypt(record.ResolutionPayload)
			var decision types.ApprovalResult
			if err != nil || record.Status != wantStatus || json.Unmarshal(plain, &decision) != nil || decision.ActorID != expectedIntrospectionActor("6492", "6492") || decision.Approved != (action == "approve") {
				t.Fatal("durable decision audit mismatch")
			}
		}
	}
}

func TestCrossTenantApprovalDecisions(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	runCrossTenantApprovalDecisions(t)
}

func TestCrossTenantApprovalHTTPSimulation(t *testing.T) {
	endpoint := os.Getenv("AI_AGENT_TEST_APPROVAL_SIM_URL")
	if endpoint == "" {
		t.Skip("explicit local HTTP identity simulation endpoint required")
	}
	installCrossTenantApprovalIdentity(t, endpoint)
	runCrossTenantApprovalDecisions(t)
}

func TestCrossTenantApprovalScopeAndRevocation(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	aid, ch := crossTenantApprovalFixture(t, st, "foreign", "7001", expectedIntrospectionActor("7001", "7001"))
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	for _, request := range []struct{ method, suffix string }{
		{"GET", ""}, {"GET", "/trace"}, {"GET", "/approvals"}, {"GET", "/stream"}, {"GET", "/workflow"},
		{"POST", "/run"}, {"POST", "/run-all"}, {"POST", "/re-audit"}, {"DELETE", "/cancel"}, {"DELETE", ""},
	} {
		response := selfApprovalRequest(router, request.method, "/api/tasks/foreign"+request.suffix, "reviewer", "{}")
		if response.Code != http.StatusNotFound {
			t.Fatalf("global approver gained %s %s: %d", request.method, request.suffix, response.Code)
		}
	}
	if response := selfApprovalRequest(router, http.MethodPost, "/api/config/reload", "reviewer", "{}"); response.Code != http.StatusForbidden {
		t.Fatalf("global approver gained admin access: %d", response.Code)
	}
	// A matching tenant's static key must never inherit the personal grant.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/foreign/approve", strings.NewReader(`{"approval_id":"`+aid+`"}`))
	req.Header.Set("X-API-Key", "local-reviewer-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("static key inherited personal authority: %d", response.Code)
	}
	// The preceding reads have cached the verified reviewer identity; revoking
	// the server-side capability must still take effect on its next request.
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		p := c.API.Tenants["6492"]
		p.ApprovalCrossTenant = false
		c.API.Tenants["6492"] = p
	}))
	if response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/foreign/approve", "reviewer", `{"approval_id":"`+aid+`"}`); response.Code != http.StatusNotFound {
		t.Fatalf("cached identity bypassed capability revocation: %d", response.Code)
	}
	select {
	case <-ch:
		t.Fatal("scope checks changed approval")
	default:
	}
}

func TestCrossTenantApprovalRequiresExactPersonalAllowlist(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	for _, p := range []Principal{
		{TenantID: "6492", ActorID: expectedIntrospectionActor("6492", "another-person")},
		{TenantID: "7002", ActorID: expectedIntrospectionActor("6492", "6492")},
		{TenantID: "6492", ActorID: "tenant:6492"},
		{TenantID: "6492", ActorID: "api-key:6492", Admin: true},
	} {
		if crossTenantApprovalAllowed(p, config.Get()) {
			t.Fatal("non-allowlisted personal identity received a cross-tenant grant")
		}
	}
}

func TestCrossTenantApprovalRevokedBeforeResolution(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	aid, ch := crossTenantApprovalFixture(t, st, "revoked-foreign", "7001", expectedIntrospectionActor("7001", "7001"))
	router := gin.New()
	h := &Handler{store: st}
	router.POST("/api/tasks/:id/approve", AuthMiddleware(), TaskTenantMiddleware(st), func(c *gin.Context) {
		t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
			p := cfg.API.Tenants["6492"]
			p.ApprovalCrossTenant = false
			cfg.API.Tenants["6492"] = p
		}))
		c.Next()
	}, ApprovalAuthorizationMiddleware(), h.approveTask)
	response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/revoked-foreign/approve", "reviewer", `{"approval_id":"`+aid+`"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked grant reached resolver: %d", response.Code)
	}
	select {
	case <-ch:
		t.Fatal("revoked grant signaled approval")
	default:
	}
}

func TestCrossTenantApprovalSelfAndUnknownCreatorFailClosed(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	router := gin.New()
	RegisterRoutes(router, st, &orchestrator.Engine{Store: st, ApprovalCodec: crossTenantApprovalCodec(t)}, nil)
	for _, action := range []string{"approve", "reject"} {
		for _, tenant := range []string{"6492", "7001"} {
			for i, creator := range []string{expectedIntrospectionActor("6492", "6492"), "", "tenant:7001"} {
				id := action + "-" + tenant + "-" + string(rune('a'+i))
				aid, ch := crossTenantApprovalFixture(t, st, id, tenant, creator)
				if err := st.CreateApproval(t.Context(), &types.DurableApproval{ID: aid, TaskID: id, TenantID: tenant, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: aid, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("fixture")}); err != nil {
					t.Fatal(err)
				}
				for _, token := range []string{"reviewer", "reviewer-refreshed"} {
					for _, body := range []string{"{}", `{"approval_id":"` + aid + `","actor_id":"forged"}`} {
						response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/"+id+"/"+action, token, body)
						if response.Code != http.StatusForbidden {
							t.Fatalf("self/unknown creator reached decision: %d", response.Code)
						}
					}
				}
				record, err := st.GetApproval(t.Context(), aid, tenant)
				if err != nil || record.Status != types.ApprovalPending || len(record.ResolutionPayload) != 0 {
					t.Fatal("forbidden decision changed durable approval")
				}
				select {
				case <-ch:
					t.Fatal("forbidden decision signaled local approval")
				default:
				}
			}
		}
	}
}

func TestCrossTenantApprovalCannotUseAnotherTaskApprovalID(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	_, _ = crossTenantApprovalFixture(t, st, "other", "7001", expectedIntrospectionActor("7001", "7001"))
	aid, ch := crossTenantApprovalFixture(t, st, "self", "6492", expectedIntrospectionActor("6492", "6492"))
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	for _, action := range []string{"approve", "reject"} {
		response := selfApprovalRequest(router, http.MethodPost, "/api/tasks/other/"+action, "reviewer", `{"approval_id":"`+aid+`"}`)
		if response.Code != http.StatusNotFound {
			t.Fatalf("another task URL bypassed self check: %d", response.Code)
		}
	}
	select {
	case <-ch:
		t.Fatal("mismatched approval ID signaled self approval")
	default:
	}
}

func TestCrossTenantApprovalConsoleCSRF(t *testing.T) {
	installCrossTenantApprovalIdentity(t, "")
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x65}, 32)))
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	aid, ch := crossTenantApprovalFixture(t, st, "console-foreign", "7001", expectedIntrospectionActor("7001", "7001"))
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	login := selfApprovalRequest(router, http.MethodPost, "/console/session", "", `{"token":"reviewer"}`)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatal("console fixture login failed")
	}
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == consoleCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("console fixture cookie missing")
	}
	for _, csrf := range []string{"", "invalid", session.CSRF} {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/console-foreign/approve", strings.NewReader(`{"approval_id":"`+aid+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Console-CSRF", csrf)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		want := http.StatusForbidden
		if csrf == session.CSRF {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("console CSRF decision: %d, want %d", response.Code, want)
		}
	}
	select {
	case result := <-ch:
		if result.ActorID != expectedIntrospectionActor("6492", "6492") {
			t.Fatal("console decision audit identity mismatch")
		}
	default:
		t.Fatal("console authorized decision missing")
	}
}
