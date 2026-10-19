package config

import (
	"os"
	"reflect"
	"testing"
)

func TestCrossTenantApprovalValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"explicit personal grant", func(*Config) {}, true},
		{"disabled", func(c *Config) { p := c.API.Tenants["6492"]; p.ApprovalCrossTenant = false; c.API.Tenants["6492"] = p }, true},
		{"no personal requirement", func(c *Config) { c.API.Auth.Bearer.RequireSubject = false }, false},
		{"no self restriction", func(c *Config) { p := c.API.Tenants["6492"]; p.ApprovalDisallowSelf = false; c.API.Tenants["6492"] = p }, false},
		{"tenant mode", func(c *Config) {
			p := c.API.Tenants["6492"]
			p.ApprovalMode = "tenant"
			p.ApprovalActorIDs = nil
			c.API.Tenants["6492"] = p
		}, false},
		{"empty allowlist", func(c *Config) { p := c.API.Tenants["6492"]; p.ApprovalActorIDs = nil; c.API.Tenants["6492"] = p }, false},
		{"raw userid", func(c *Config) {
			p := c.API.Tenants["6492"]
			p.ApprovalActorIDs = []string{"6492"}
			c.API.Tenants["6492"] = p
		}, false},
		{"static only", func(c *Config) { c.API.Auth.Mode = "api_key" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.LLM.Provider, c.LLM.TimeoutSeconds = "openai", 30
			c.API.Auth = APIAuthConfig{Mode: "introspection", Bearer: APIBearerConfig{RequireSubject: true}, Introspection: APIIntrospectionConfig{
				URL: "https://identity.example.invalid/check", TenantClaim: "data.userid", SubjectClaim: "data.userid", ActiveClaim: "data.active", TimeoutSeconds: 3,
			}}
			c.API.Tenants = map[string]APITenantConfig{"6492": {ApprovalCrossTenant: true, ApprovalDisallowSelf: true, ApprovalMode: "actors", ApprovalActorIDs: []string{testPersonalActor}}}
			tc.mutate(c)
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("validation success=%v, expected %v", err == nil, tc.ok)
			}
		})
	}
}

func TestCrossTenantApprovalReload(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	write := func(enabled, self string) {
		t.Helper()
		body := append(brainReloadConfig("./data/brain"), []byte(`
api:
  auth:
    mode: introspection
    bearer:
      require_subject: true
    introspection:
      url: https://identity.example.invalid/check
      tenant_claim: data.userid
      subject_claim: data.userid
      active_claim: data.active
      timeout_seconds: 3
  tenants:
    "6492":
      approval_mode: actors
      approval_actor_ids: ["`+testPersonalActor+`"]
      approval_disallow_self: `+self+`
      approval_cross_tenant: `+enabled+`
`)...)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("true", "true")
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	before, revision := Get(), Revision()
	if !before.API.Tenants["6492"].ApprovalCrossTenant {
		t.Fatal("grant was not loaded")
	}
	cloned := cloneConfig(before)
	p := cloned.API.Tenants["6492"]
	p.ApprovalActorIDs[0] = "changed"
	p.ApprovalCrossTenant = false
	cloned.API.Tenants["6492"] = p
	if !reflect.DeepEqual(before.API.Tenants["6492"].ApprovalActorIDs, []string{testPersonalActor}) || !before.API.Tenants["6492"].ApprovalCrossTenant {
		t.Fatal("cloning changed active approval policy")
	}
	for _, candidate := range [][2]string{{"true", "false"}, {"not-a-boolean", "true"}} {
		write(candidate[0], candidate[1])
		if _, _, err := Reload(); err == nil {
			t.Fatal("unsafe candidate accepted")
		}
		if Get() != before || Revision() != revision {
			t.Fatal("rejected candidate changed active snapshot")
		}
	}
	write("false", "true")
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	if Get().API.Tenants["6492"].ApprovalCrossTenant {
		t.Fatal("grant was not revoked")
	}
}
