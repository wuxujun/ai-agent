package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Separate opt-in: ordinary tests neither load .env nor contact real identity
// services. This entry does not enable or alter any deployed configuration.
func TestExternalCrossTenantAuthAcceptance(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_CROSS_TENANT_AUTH_ACCEPTANCE") != "true" {
		t.Skip("explicit cross-tenant acceptance opt-in and reviewed credentials required")
	}
	ids := crossTenantAcceptanceIDs{
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_TENANT"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_REVIEWER_TENANT"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_UNLISTED_TENANT"),
	}
	path := os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_CONFIG")
	if path == "" {
		t.Fatal("reviewed acceptance configuration is required")
	}
	v := viper.New()
	v.SetConfigFile(path)
	if v.ReadInConfig() != nil {
		t.Fatal("cannot read acceptance configuration")
	}
	cfg := &config.Config{}
	if v.Unmarshal(cfg) != nil {
		t.Fatal("cannot decode acceptance configuration")
	}
	if err := validateCrossTenantAuthAcceptanceConfig(cfg, ids); err != nil {
		t.Fatal(err)
	}
	tokens := authAcceptanceTokens{
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_CREATOR_TOKEN"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_REFRESHED_TOKEN"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_REVIEWER_TOKEN"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_UNLISTED_TOKEN"),
		os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_EXPIRED_TOKEN"),
	}
	if err := tokens.validate(); err != nil {
		t.Fatal(err)
	}
	runCrossTenantAuthAcceptance(t, cfg, ids, tokens)
}

type crossTenantAcceptanceIDs struct{ creator, reviewer, unlisted string }

func validateCrossTenantAuthAcceptanceConfig(cfg *config.Config, ids crossTenantAcceptanceIDs) error {
	if ids.creator == "" || ids.reviewer == "" || ids.unlisted == "" || ids.creator == ids.reviewer || ids.creator == ids.unlisted || ids.reviewer == ids.unlisted {
		return errors.New("three distinct expected user-scoped tenant IDs are required")
	}
	// All three identities have explicit isolation scopes and allowlisted personal
	// actors. The unlisted user is unlisted for CROSS-tenant decisions, not unknown
	// to authentication. This prevents a 401 from masquerading as authorization.
	for _, id := range []string{ids.creator, ids.reviewer, ids.unlisted} {
		if err := validateProductionAuthAcceptanceBase(cfg, id, true); err != nil {
			return err
		}
		p := Principal{TenantID: id, ActorID: expectedIntrospectionActor(id, id)}
		tenant := cfg.API.Tenants[id]
		listed := false
		for _, actor := range tenant.ApprovalActorIDs {
			listed = listed || actor == p.ActorID
		}
		if !listed || crossTenantApprovalAllowed(p, cfg) != (id == ids.reviewer) {
			return errors.New("reviewed user allowlists must grant cross-tenant decisions only to the reviewer role")
		}
	}
	return nil
}

func runCrossTenantAuthAcceptance(t *testing.T, cfg *config.Config, ids crossTenantAcceptanceIDs, tokens authAcceptanceTokens) {
	t.Helper()
	mode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(mode) })
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.API = cfg.API }))
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 32)))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	codec := crossTenantApprovalCodec(t)
	router := gin.New()
	RegisterRoutes(router, st, &orchestrator.Engine{Store: st, ApprovalCodec: codec}, nil)
	router.GET("/acceptance/identity", AuthMiddleware(), func(c *gin.Context) { c.JSON(http.StatusOK, principalFromGin(c)) })
	request := func(method, path, token string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal("cannot encode acceptance request")
			}
		}
		req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(data))
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-Console-CSRF", csrf)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	requireStatus := func(r *httptest.ResponseRecorder, want int, label string) {
		t.Helper()
		if r.Code != want {
			t.Fatalf("%s: status %d, expected %d (response redacted)", label, r.Code, want)
		}
	}
	for _, role := range []struct{ token, id string }{{tokens.creator, ids.creator}, {tokens.refreshed, ids.creator}, {tokens.reviewer, ids.reviewer}, {tokens.unlisted, ids.unlisted}} {
		r := request("GET", "/acceptance/identity", role.token, nil, nil, "")
		requireStatus(r, 200, "verified identity")
		var p Principal
		if json.Unmarshal(r.Body.Bytes(), &p) != nil || p.TenantID != role.id || p.ActorID != expectedIntrospectionActor(role.id, role.id) || p.Admin {
			t.Fatal("identity did not match reviewed shared-userid principal")
		}
	}
	for _, token := range []string{tokens.expired, ""} {
		requireStatus(request("GET", "/acceptance/identity", token, nil, nil, ""), 401, "invalid credential")
	}
	login := request("POST", "/console/session", "", map[string]string{"token": tokens.reviewer}, nil, "")
	requireStatus(login, 200, "reviewer Console login")
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(login.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatal("missing Console CSRF protection")
	}
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == consoleCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("missing HttpOnly Console session")
	}
	// Persist and decrypt decisions in Memory Store; this is not target-database
	// durability or live task execution. No pending task is run by this harness.
	for _, viaConsole := range []bool{false, true} {
		for _, action := range []string{"approve", "reject"} {
			id := "cross-auth-" + action + map[bool]string{false: "-api", true: "-console"}[viaConsole]
			create := map[string]string{"id": id, "goal": "identity acceptance fixture only", "workspace": cfg.API.Tenants[ids.creator].WorkspaceRoot, "creator_actor_id": "forged"}
			requireStatus(request("POST", "/api/tasks", tokens.creator, create, nil, ""), 201, "creator capture")
			task, err := st.GetTask(ctx, id)
			if err != nil || task.CreatorActorID != expectedIntrospectionActor(ids.creator, ids.creator) || task.TenantID != ids.creator {
				t.Fatal("verified creator was not captured")
			}
			aid, ch := orchestrator.RegisterApproval(id, &types.ApprovalRequest{TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh})
			t.Cleanup(func() { orchestrator.RemoveApproval(aid) })
			if st.CreateApproval(ctx, &types.DurableApproval{ID: aid, TaskID: id, TenantID: ids.creator, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: aid, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("acceptance fixture")}) != nil {
				t.Fatal("cannot create durable approval fixture")
			}
			body := map[string]string{"approval_id": aid, "actor_id": "forged"}
			path := "/api/tasks/" + id + "/" + action
			for _, attempt := range []struct {
				token  string
				status int
			}{{tokens.creator, 403}, {tokens.refreshed, 403}, {tokens.unlisted, 404}, {tokens.expired, 401}, {"", 401}} {
				requireStatus(request("POST", path, attempt.token, body, nil, ""), attempt.status, "unauthorized decision")
			}
			for _, suffix := range []string{"", "/trace", "/approvals", "/stream"} {
				requireStatus(request("GET", "/api/tasks/"+id+suffix, tokens.reviewer, nil, nil, ""), 404, "reviewer read isolation")
			}
			if viaConsole {
				for _, csrf := range []string{"", "invalid"} {
					requireStatus(request("POST", path, "", body, cookie, csrf), 403, "Console CSRF")
				}
			}
			select {
			case <-ch:
				t.Fatal("forbidden request signaled decision")
			default:
			}
			before, err := st.GetApproval(ctx, aid, ids.creator)
			if err != nil || before.Status != types.ApprovalPending || len(before.ResolutionPayload) != 0 {
				t.Fatal("forbidden request mutated durable decision")
			}
			token, csrf := tokens.reviewer, ""
			var useCookie *http.Cookie
			if viaConsole {
				token = ""
				csrf = session.CSRF
				useCookie = cookie
			}
			requireStatus(request("POST", path, token, body, useCookie, csrf), 200, "independent decision")
			select {
			case result := <-ch:
				if result.ActorID != expectedIntrospectionActor(ids.reviewer, ids.reviewer) || result.Approved != (action == "approve") {
					t.Fatal("decision delivery identity mismatch")
				}
			default:
				t.Fatal("authorized decision not delivered")
			}
			record, err := st.GetApproval(ctx, aid, ids.creator)
			if err != nil {
				t.Fatal("cannot read durable decision")
			}
			plain, err := codec.Decrypt(record.ResolutionPayload)
			var result types.ApprovalResult
			want := types.ApprovalRejected
			if action == "approve" {
				want = types.ApprovalApproved
			}
			if err != nil || json.Unmarshal(plain, &result) != nil || record.Status != want || result.ActorID != expectedIntrospectionActor(ids.reviewer, ids.reviewer) || result.Approved != (action == "approve") {
				t.Fatal("durable decision identity mismatch")
			}
		}
	}
	// Self decisions and decisions after local capability revocation must fail.
	aid, ch := crossTenantApprovalFixture(t, st, "cross-auth-self", ids.reviewer, expectedIntrospectionActor(ids.reviewer, ids.reviewer))
	body := map[string]string{"approval_id": aid}
	for _, action := range []string{"approve", "reject"} {
		requireStatus(request("POST", "/api/tasks/cross-auth-self/"+action, tokens.reviewer, body, nil, ""), 403, "reviewer self decision")
	}
	foreignID, foreignCh := crossTenantApprovalFixture(t, st, "cross-auth-revoked", ids.creator, expectedIntrospectionActor(ids.creator, ids.creator))
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		tenant := c.API.Tenants[ids.reviewer]
		tenant.ApprovalCrossTenant = false
		c.API.Tenants[ids.reviewer] = tenant
	}))
	for _, action := range []string{"approve", "reject"} {
		requireStatus(request("POST", "/api/tasks/cross-auth-revoked/"+action, tokens.reviewer, map[string]string{"approval_id": foreignID}, nil, ""), 404, "local capability revocation")
	}
	for _, pending := range []<-chan types.ApprovalResult{ch, foreignCh} {
		select {
		case <-pending:
			t.Fatal("self/revoked decision delivered")
		default:
		}
	}
	t.Log("shared-userid identities, API/Console cross-tenant decisions, self denial, local revocation and encrypted Memory Store records passed; no deployment or task execution")
}

func crossTenantAuthAcceptanceFixtureConfig() (*config.Config, crossTenantAcceptanceIDs) {
	cfg := authAcceptanceFixtureConfig()
	cfg.API.Auth.Introspection.SubjectClaim = "data.userid"
	ids := crossTenantAcceptanceIDs{"7001", "6492", "7002"}
	cfg.API.Tenants = map[string]config.APITenantConfig{}
	for _, id := range []string{ids.creator, ids.reviewer, ids.unlisted} {
		cfg.API.Tenants[id] = config.APITenantConfig{
			WorkspaceRoot: "./testdata", AllowedMultiAgentTeams: []string{"kb_qa"}, WikiSpace: "user-" + id,
			ApprovalMode: "actors", ApprovalDisallowSelf: true, ApprovalActorIDs: []string{expectedIntrospectionActor(id, id)}, ApprovalCrossTenant: id == ids.reviewer,
		}
	}
	return cfg, ids
}

func TestCrossTenantAuthAcceptancePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, *crossTenantAcceptanceIDs)
		ok     bool
	}{
		{"reviewed shared userid", func(*config.Config, *crossTenantAcceptanceIDs) {}, true},
		{"duplicate users", func(_ *config.Config, ids *crossTenantAcceptanceIDs) { ids.reviewer = ids.creator }, false},
		{"different claims", func(c *config.Config, _ *crossTenantAcceptanceIDs) {
			c.API.Auth.Introspection.SubjectClaim = "data.sub"
		}, false},
		{"unreviewed shared field", func(c *config.Config, _ *crossTenantAcceptanceIDs) {
			c.API.Auth.Introspection.SubjectClaim = "sub"
			c.API.Auth.Introspection.TenantClaim = "sub"
		}, false},
		{"missing reviewer grant", func(c *config.Config, ids *crossTenantAcceptanceIDs) {
			p := c.API.Tenants[ids.reviewer]
			p.ApprovalCrossTenant = false
			c.API.Tenants[ids.reviewer] = p
		}, false},
		{"unlisted grant", func(c *config.Config, ids *crossTenantAcceptanceIDs) {
			p := c.API.Tenants[ids.unlisted]
			p.ApprovalCrossTenant = true
			c.API.Tenants[ids.unlisted] = p
		}, false},
		{"wrong actor", func(c *config.Config, ids *crossTenantAcceptanceIDs) {
			p := c.API.Tenants[ids.reviewer]
			p.ApprovalActorIDs = []string{expectedIntrospectionActor(ids.creator, ids.creator)}
			c.API.Tenants[ids.reviewer] = p
		}, false},
		{"missing user", func(c *config.Config, ids *crossTenantAcceptanceIDs) { delete(c.API.Tenants, ids.unlisted) }, false},
		{"cache", func(c *config.Config, _ *crossTenantAcceptanceIDs) { c.API.Auth.Introspection.CacheTTLSeconds = 1 }, false},
		{"private network", func(c *config.Config, _ *crossTenantAcceptanceIDs) {
			c.API.Auth.Introspection.AllowPrivateNetwork = true
		}, false},
		{"static key", func(c *config.Config, _ *crossTenantAcceptanceIDs) { c.API.APIKey = "secret" }, false},
		{"no subject", func(c *config.Config, _ *crossTenantAcceptanceIDs) { c.API.Auth.Bearer.RequireSubject = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, ids := crossTenantAuthAcceptanceFixtureConfig()
			tc.mutate(cfg, &ids)
			if (validateCrossTenantAuthAcceptanceConfig(cfg, ids) == nil) != tc.ok {
				t.Fatal("unexpected preflight result")
			}
		})
	}
	cfg, ids := crossTenantAuthAcceptanceFixtureConfig()
	if validateProductionAuthAcceptanceConfig(cfg, ids.creator) == nil {
		t.Fatal("original acceptance entry admitted shared claims")
	}
}

func TestCrossTenantAuthAcceptanceFixture(t *testing.T) {
	cfg, ids := crossTenantAuthAcceptanceFixtureConfig()
	if err := validateCrossTenantAuthAcceptanceConfig(cfg, ids); err != nil {
		t.Fatal(err)
	}
	installTestIntrospector(t, cfg.API.Auth.Introspection, nil, func(req *http.Request) (int, string) {
		if req.Header.Get("X-API-Key") != "" {
			t.Error("static key forwarded")
		}
		id, active, exp := ids.creator, true, expirationAfter(time.Minute)
		switch req.Header.Get("Authorization") {
		case "Bearer creator", "Bearer refreshed":
		case "Bearer reviewer":
			id = ids.reviewer
		case "Bearer unlisted":
			id = ids.unlisted
		case "Bearer expired":
			exp = expirationAfter(-time.Minute)
		default:
			active = false
		}
		body, _ := json.Marshal(map[string]any{"iss": cfg.API.Auth.Introspection.Issuer, "data": map[string]any{"userid": id, "active": active, "aud": cfg.API.Auth.Introspection.Audience, "exp": json.Number(exp), "admin": true}})
		return 200, string(body)
	})
	runCrossTenantAuthAcceptance(t, cfg, ids, authAcceptanceTokens{"creator", "refreshed", "reviewer", "unlisted", "expired"})
}
