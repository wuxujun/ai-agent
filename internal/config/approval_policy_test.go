package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

const testPersonalActor = "introspection-sub:0123456789abcdef01234567"

func TestApprovalPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		mode string
		ids  []string
		ok   bool
	}{
		{"", nil, true}, {"tenant", nil, true}, {"deny", nil, true},
		{"actors", []string{testPersonalActor}, true},
		{"", []string{testPersonalActor}, false}, {"tenant", []string{testPersonalActor}, false},
		{"actors", nil, false}, {"roles", nil, false},
		{"actors", []string{"tenant:project-a"}, false},
		{"actors", []string{testPersonalActor, testPersonalActor}, false},
		{"actors", []string{"introspection-sub:INVALID"}, false},
	} {
		cfg := &Config{}
		cfg.LLM.Provider, cfg.LLM.TimeoutSeconds = "openai", 30
		cfg.API.Auth.Mode = "introspection"
		cfg.API.Auth.Introspection = APIIntrospectionConfig{URL: "https://identity.example.invalid/check", TenantClaim: "tenant", ActiveClaim: "active", TimeoutSeconds: 3}
		cfg.API.Tenants = map[string]APITenantConfig{"project-a": {ApprovalMode: tc.mode, ApprovalActorIDs: tc.ids}}
		if err := cfg.Validate(); (err == nil) != tc.ok {
			t.Fatalf("mode=%q ids=%v validation=%v", tc.mode, tc.ids, err)
		}
	}
}

func TestApprovalPolicyReloadPreservesRejectedSnapshot(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	write := func(mode string, disallow string) {
		t.Helper()
		body := append(brainReloadConfig("./data/brain"), []byte("\napi:\n  tenants:\n    project-a:\n      api_key: test-local-key\n      approval_mode: "+mode+"\n      approval_actor_ids: ["+testPersonalActor+"]\n      approval_disallow_self: "+disallow+"\n")...)
		if err := os.WriteFile(path, body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("actors", "true")
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	before, revision := Get(), Revision()
	if !before.API.Tenants["project-a"].ApprovalDisallowSelf {
		t.Fatal("self-approval policy not loaded")
	}
	cloned := cloneConfig(before)
	tenant := cloned.API.Tenants["project-a"]
	tenant.ApprovalActorIDs[0] = "changed"
	if !reflect.DeepEqual(before.API.Tenants["project-a"].ApprovalActorIDs, []string{testPersonalActor}) {
		t.Fatal("cloning shared the approval allowlist backing array")
	}
	write("invalid-mode", "false")
	if _, _, err := Reload(); err == nil {
		t.Fatal("invalid policy reload accepted")
	}
	if Get() != before || Revision() != revision {
		t.Fatal("rejected reload replaced the active snapshot")
	}
	write("actors", "not-a-boolean")
	if _, _, err := Reload(); err == nil {
		t.Fatal("invalid self-approval policy accepted")
	}
	if Get() != before || Revision() != revision {
		t.Fatal("invalid self-approval policy replaced snapshot")
	}
	write("deny", "false")
	if _, _, err := Reload(); err != nil || Get().API.Tenants["project-a"].ApprovalMode != "deny" || Get().API.Tenants["project-a"].ApprovalDisallowSelf {
		t.Fatalf("valid deny reload failed: %v", err)
	}
}

func TestProductionAuthExampleAndSubjectEnvBinding(t *testing.T) {
	resetConfig()
	t.Cleanup(resetConfig)
	setupViper()
	viper.SetConfigFile("../../deploy/security/config.production.example.yaml")
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	cfg, err := unmarshalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("production example validation: %v", err)
	}
	if !cfg.API.Auth.Bearer.RequireSubject || !cfg.API.Auth.RequireTenantWorkspaceRoot || !cfg.API.Auth.Introspection.RequireKnownTenant || !cfg.API.Auth.Introspection.RequireExpiration || cfg.API.Tenants["project-a"].ApprovalMode != "deny" || !cfg.API.Tenants["project-a"].ApprovalDisallowSelf {
		t.Fatal("production example weakened required identity or isolation")
	}
	cfg.API.Auth.Introspection.SubjectClaim = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "subject_claim") {
		t.Fatalf("required identity without a subject path: %v", err)
	}
	t.Setenv("AI_AGENT_API_INTROSPECTION_SUBJECT_CLAIM", "data.person_id")
	t.Setenv("AI_AGENT_API_BEARER_REQUIRE_SUBJECT", "true")
	cfg, err = unmarshalConfig()
	if err != nil || cfg.API.Auth.Introspection.SubjectClaim != "data.person_id" || !cfg.API.Auth.Bearer.RequireSubject {
		t.Fatalf("identity env binding failed: %v", err)
	}
}
