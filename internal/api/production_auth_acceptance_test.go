package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Explicit opt-in: never load .env or run against an identity provider in the
// ordinary suite. Credentials are environment inputs, never command arguments,
// assertions, request/response dumps, or artifacts. All tasks remain in memory.
func TestExternalProductionAuthAcceptance(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_AUTH_ACCEPTANCE") != "true" {
		t.Skip("set AI_AGENT_RUN_AUTH_ACCEPTANCE=true with a reviewed config and test credentials")
	}
	path := os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_CONFIG")
	tenant := os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_TENANT")
	if path == "" || tenant == "" {
		t.Fatal("acceptance config and expected tenant are required")
	}
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatal("cannot read acceptance configuration")
	}
	cfg := &config.Config{}
	if err := v.Unmarshal(cfg); err != nil {
		t.Fatal("cannot decode acceptance configuration")
	}
	if err := validateProductionAuthAcceptanceConfig(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	tokens := authAcceptanceTokens{
		creator:   os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_CREATOR_TOKEN"),
		refreshed: os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_REFRESHED_TOKEN"),
		reviewer:  os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_REVIEWER_TOKEN"),
		unlisted:  os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_UNLISTED_TOKEN"),
		expired:   os.Getenv("AI_AGENT_AUTH_ACCEPTANCE_EXPIRED_TOKEN"),
	}
	if err := tokens.validate(); err != nil {
		t.Fatal(err)
	}
	runProductionAuthAcceptance(t, cfg, tenant, tokens)
}

type authAcceptanceTokens struct{ creator, refreshed, reviewer, unlisted, expired string }

func (tokens authAcceptanceTokens) validate() error {
	seen := map[string]bool{}
	for _, token := range []string{tokens.creator, tokens.refreshed, tokens.reviewer, tokens.unlisted, tokens.expired} {
		if token == "" || len(token) > 2500 || strings.ContainsFunc(token, unicode.IsSpace) || strings.ContainsFunc(token, unicode.IsControl) || seen[token] {
			return errors.New("five distinct nonempty test credentials within Console size limits are required")
		}
		seen[token] = true
	}
	return nil
}

func validateProductionAuthAcceptanceConfig(cfg *config.Config, tenantID string) error {
	auth := cfg.API.Auth
	if auth.Mode != "introspection" || !auth.Bearer.RequireSubject || !auth.RequireTenantWorkspaceRoot {
		return errors.New("acceptance requires introspection, personal subjects, and tenant workspace isolation")
	}
	in := auth.Introspection
	if in.SubjectClaim == "" || in.SubjectClaim == in.TenantClaim || in.Issuer == "" || in.Audience == "" || !in.RequireExpiration || !in.RequireKnownTenant || in.CacheTTLSeconds != 0 || in.AllowPrivateNetwork {
		return errors.New("acceptance requires distinct subject and tenant fields, issuer/audience/expiration/known-tenant checks, no cache, and public-network policy")
	}
	parsed, err := url.Parse(in.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("acceptance requires a reviewed HTTPS identity endpoint")
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.HasSuffix(host, ".invalid") || strings.HasSuffix(host, ".example") || host == "example.com" || strings.HasSuffix(host, ".example.com") {
		return errors.New("acceptance identity endpoint is still a placeholder")
	}
	if cfg.API.APIKey != "" {
		return errors.New("acceptance forbids a root static management key")
	}
	for _, tenant := range cfg.API.Tenants {
		if tenant.Admin || tenant.APIKey != "" {
			return errors.New("acceptance requires non-admin tenants without static keys")
		}
	}
	tenant, ok := cfg.API.Tenants[tenantID]
	if !ok || tenant.WorkspaceRoot == "" || len(tenant.AllowedMultiAgentTeams) == 0 || tenant.WikiSpace == "" || !tenant.ApprovalDisallowSelf || tenant.ApprovalMode != "actors" {
		return errors.New("expected tenant requires explicit workspace/Team/Wiki scopes, actors mode, and self-approval prohibition")
	}
	if policy.ValidateWorkspace(tenant.WorkspaceRoot) != nil {
		return errors.New("acceptance tenant workspace is outside supported workspace boundaries")
	}
	// Validate the auth/tenant candidate independently of unrelated LLM/storage
	// defaults. This does not apply environment overlays or deploy the file.
	check := &config.Config{}
	check.API = cfg.API
	check.LLM.Provider, check.LLM.TimeoutSeconds = "openai", 30
	if check.Validate() != nil {
		return errors.New("acceptance auth/tenant configuration is invalid")
	}
	return nil
}

func runProductionAuthAcceptance(t *testing.T, cfg *config.Config, tenantID string, tokens authAcceptanceTokens) {
	t.Helper()
	previousMode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.API = cfg.API }))
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6a}, 32)))
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	router := gin.New()
	RegisterRoutes(router, st, nil, nil)
	router.GET("/acceptance/identity", AuthMiddleware(), func(c *gin.Context) { c.JSON(http.StatusOK, principalFromGin(c)) })
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	request := func(method, path, token string, body []byte, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
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
	requireStatus := func(response *httptest.ResponseRecorder, want int, label string) {
		t.Helper()
		if response.Code != want {
			t.Fatalf("%s: status %d, expected %d (response redacted)", label, response.Code, want)
		}
	}
	identity := func(token, label string) Principal {
		t.Helper()
		response := request(http.MethodGet, "/acceptance/identity", token, nil, nil, "")
		requireStatus(response, http.StatusOK, label)
		var p Principal
		if json.Unmarshal(response.Body.Bytes(), &p) != nil || p.TenantID != tenantID || p.Admin || !(bearerIdentity{ActorID: p.ActorID}).personal() {
			t.Fatal("identity contract did not establish the expected non-admin personal tenant principal")
		}
		return p
	}
	creator := identity(tokens.creator, "creator identity")
	refreshed := identity(tokens.refreshed, "refreshed identity")
	reviewer := identity(tokens.reviewer, "reviewer identity")
	unlisted := identity(tokens.unlisted, "unlisted identity")
	if creator.ActorID != refreshed.ActorID {
		t.Fatal("creator personal identity changed after token refresh")
	}
	if creator.ActorID == reviewer.ActorID || creator.ActorID == unlisted.ActorID || reviewer.ActorID == unlisted.ActorID {
		t.Fatal("independent test users did not yield distinct personal identities")
	}
	listed := func(actor string) bool {
		for _, id := range cfg.API.Tenants[tenantID].ApprovalActorIDs {
			if id == actor {
				return true
			}
		}
		return false
	}
	if !listed(reviewer.ActorID) || listed(unlisted.ActorID) {
		t.Fatal("reviewer/unlisted credentials do not match the reviewed approval allowlist")
	}
	requireStatus(request(http.MethodGet, "/acceptance/identity", tokens.expired, nil, nil, ""), http.StatusUnauthorized, "expired credential")
	requireStatus(request(http.MethodGet, "/acceptance/identity", "", nil, nil, ""), http.StatusUnauthorized, "missing credential")
	// Validate the real Console login path without exposing its cookie or token.
	loginBody, _ := json.Marshal(map[string]string{"token": tokens.creator})
	login := request(http.MethodPost, "/console/session", "", loginBody, nil, "")
	requireStatus(login, http.StatusOK, "Console login")
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(login.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatal("Console login did not supply CSRF protection")
	}
	var cookie *http.Cookie
	for _, item := range login.Result().Cookies() {
		if item.Name == consoleCookieName {
			cookie = item
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("Console login did not supply an HttpOnly session")
	}
	for _, viaConsole := range []bool{false, true} {
		id := "production-auth-api-fixture"
		token := tokens.creator
		var useCookie *http.Cookie
		csrf := ""
		if viaConsole {
			id = "production-auth-console-fixture"
			token = ""
			useCookie = cookie
			csrf = session.CSRF
		}
		body, _ := json.Marshal(map[string]string{"id": id, "goal": "identity acceptance only", "workspace": cfg.API.Tenants[tenantID].WorkspaceRoot, "creator_actor_id": reviewer.ActorID})
		if viaConsole {
			requireStatus(request(http.MethodPost, "/api/tasks", token, body, useCookie, ""), http.StatusForbidden, "Console missing CSRF")
		}
		requireStatus(request(http.MethodPost, "/api/tasks", token, body, useCookie, csrf), http.StatusCreated, "authenticated creation")
		task, err := st.GetTask(t.Context(), id)
		if err != nil || task.CreatorActorID != creator.ActorID {
			t.Fatal("authenticated creation failed to retain the verified creator")
		}
		for _, action := range []string{"approve", "reject"} {
			approvalID, ch := orchestrator.RegisterApproval(id, &types.ApprovalRequest{TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh})
			t.Cleanup(func() { orchestrator.RemoveApproval(approvalID) })
			decision, _ := json.Marshal(map[string]string{"approval_id": approvalID, "actor_id": reviewer.ActorID})
			path := "/api/tasks/" + id + "/" + action
			for _, attempt := range []struct {
				token, label string
				status       int
			}{
				{tokens.creator, "self decision", http.StatusForbidden}, {tokens.refreshed, "refreshed self decision", http.StatusForbidden},
				{tokens.unlisted, "unlisted decision", http.StatusForbidden}, {tokens.expired, "expired decision", http.StatusUnauthorized},
			} {
				requireStatus(request(http.MethodPost, path, attempt.token, decision, nil, ""), attempt.status, attempt.label)
			}
			select {
			case <-ch:
				t.Fatal("a forbidden decision signaled the local approval")
			default:
			}
			requireStatus(request(http.MethodGet, "/api/tasks/"+id+"/approvals", tokens.unlisted, nil, nil, ""), http.StatusOK, "unlisted user read permission")
			requireStatus(request(http.MethodPost, path, tokens.reviewer, decision, nil, ""), http.StatusOK, "independent reviewer decision")
			select {
			case result := <-ch:
				if result.ActorID != reviewer.ActorID || result.Approved != (action == "approve") {
					t.Fatal("independent decision audit identity mismatch")
				}
			default:
				t.Fatal("authorized decision was not delivered")
			}
		}
	}
	t.Log("identity stability, fail-closed expiry, API/Console creator capture, CSRF, self-approval and local allowlist contract passed; no deployment or LLM execution")
}

func authAcceptanceFixtureConfig() *config.Config {
	cfg := &config.Config{}
	cfg.API.Auth = config.APIAuthConfig{Mode: "introspection", RequireTenantWorkspaceRoot: true, Bearer: config.APIBearerConfig{RequireSubject: true}, Introspection: testIntrospectionConfig()}
	cfg.API.Auth.Introspection.URL = "https://api.thinktown.com/api/auth/check"
	cfg.API.Auth.Introspection.SubjectClaim = "data.sub"
	cfg.API.Auth.Introspection.CacheTTLSeconds = 0
	cfg.API.Auth.Introspection.RequireKnownTenant = true
	cfg.API.Auth.Introspection.RequireExpiration = true
	cfg.API.Tenants = map[string]config.APITenantConfig{"project_a": {
		WorkspaceRoot: "./testdata", AllowedMultiAgentTeams: []string{"kb_qa"}, WikiSpace: "project-a",
		ApprovalMode: "actors", ApprovalDisallowSelf: true,
		ApprovalActorIDs: []string{expectedIntrospectionActor("project_a", "creator"), expectedIntrospectionActor("project_a", "reviewer")},
	}}
	return cfg
}

func TestProductionAuthAcceptancePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		ok     bool
	}{
		{"strict candidate", func(*config.Config) {}, true},
		{"compatibility mode", func(c *config.Config) { c.API.Auth.Mode = "hybrid" }, false},
		{"no personal requirement", func(c *config.Config) { c.API.Auth.Bearer.RequireSubject = false }, false},
		{"same tenant and subject", func(c *config.Config) { c.API.Auth.Introspection.SubjectClaim = c.API.Auth.Introspection.TenantClaim }, false},
		{"no issuer", func(c *config.Config) { c.API.Auth.Introspection.Issuer = "" }, false},
		{"no audience", func(c *config.Config) { c.API.Auth.Introspection.Audience = "" }, false},
		{"no expiration", func(c *config.Config) { c.API.Auth.Introspection.RequireExpiration = false }, false},
		{"unknown tenants", func(c *config.Config) { c.API.Auth.Introspection.RequireKnownTenant = false }, false},
		{"cache enabled", func(c *config.Config) { c.API.Auth.Introspection.CacheTTLSeconds = 10 }, false},
		{"private networks", func(c *config.Config) { c.API.Auth.Introspection.AllowPrivateNetwork = true }, false},
		{"placeholder endpoint", func(c *config.Config) { c.API.Auth.Introspection.URL = "https://identity.example.invalid/auth/check" }, false},
		{"static root key", func(c *config.Config) { c.API.APIKey = "never-print-fixture-secret" }, false},
		{"admin tenant", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.Admin = true
			c.API.Tenants["project_a"] = tenant
		}, false},
		{"static tenant key", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.APIKey = "never-print-fixture-secret"
			c.API.Tenants["project_a"] = tenant
		}, false},
		{"no self policy", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.ApprovalDisallowSelf = false
			c.API.Tenants["project_a"] = tenant
		}, false},
		{"deny mode", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.ApprovalMode = "deny"
			c.API.Tenants["project_a"] = tenant
		}, false},
		{"invalid allowlist", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.ApprovalActorIDs = []string{"never-print-fixture-secret"}
			c.API.Tenants["project_a"] = tenant
		}, false},
		{"unsafe workspace", func(c *config.Config) {
			tenant := c.API.Tenants["project_a"]
			tenant.WorkspaceRoot = "/"
			c.API.Tenants["project_a"] = tenant
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := authAcceptanceFixtureConfig()
			tc.mutate(cfg)
			err := validateProductionAuthAcceptanceConfig(cfg, "project_a")
			if (err == nil) != tc.ok {
				t.Fatalf("preflight valid=%v, expected %v", err == nil, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "never-print-fixture-secret") {
				t.Fatal("preflight error exposed configured secret")
			}
		})
	}
}

func TestProductionAuthAcceptanceCredentialInputs(t *testing.T) {
	valid := authAcceptanceTokens{"creator", "refreshed", "reviewer", "unlisted", "expired"}
	if valid.validate() != nil {
		t.Fatal("valid credential set rejected")
	}
	for _, value := range []string{"", "creator", "contains space", "newline\n", "\x00control", strings.Repeat("x", 2501)} {
		invalid := valid
		invalid.reviewer = value
		if invalid.validate() == nil {
			t.Fatal("invalid credential set accepted")
		}
	}
}

func TestProductionAuthAcceptanceFixture(t *testing.T) {
	cfg := authAcceptanceFixtureConfig()
	if err := validateProductionAuthAcceptanceConfig(cfg, "project_a"); err != nil {
		t.Fatal(err)
	}
	installTestIntrospector(t, cfg.API.Auth.Introspection, nil, func(req *http.Request) (int, string) {
		if req.Header.Get("X-API-Key") != "" {
			t.Error("static key was forwarded")
		}
		subject := "creator"
		expiration := expirationAfter(time.Minute)
		switch req.Header.Get("Authorization") {
		case "Bearer reviewer":
			subject = "reviewer"
		case "Bearer unlisted":
			subject = "unlisted"
		case "Bearer expired":
			expiration = expirationAfter(-time.Minute)
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"project_a","sub":"` + subject + `","aud":"ai-agent","exp":` + expiration + `}}`
	})
	runProductionAuthAcceptance(t, cfg, "project_a", authAcceptanceTokens{"creator", "refreshed", "reviewer", "unlisted", "expired"})
}
