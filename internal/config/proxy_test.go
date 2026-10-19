package config

import (
	"errors"
	"os"
	"testing"
)

func TestTrustedProxyValidationAndClone(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"127.0.0.1", true}, {"10.0.0.0/24", true}, {"::1", true}, {"proxy.example", false}, {"10.0.0.1/99", false}, {"", false}} {
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		cfg.LLM.TimeoutSeconds = 30
		cfg.API.TrustedProxies = []string{tc.value}
		if err := cfg.Validate(); (err == nil) != tc.valid {
			t.Fatalf("%q: %v", tc.value, err)
		}
		cloned := cloneConfig(cfg)
		cloned.API.TrustedProxies[0] = "changed"
		if cfg.API.TrustedProxies[0] != tc.value {
			t.Fatal("proxy list aliased")
		}
	}
}
func TestTrustedProxyReloadPreservesSnapshot(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	initial := Get()
	revision := Revision()
	body := append(brainReloadConfig("./data/brain"), []byte("\napi:\n  trusted_proxies: [127.0.0.1]\n")...)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("reload: %v", err)
	}
	if Get() != initial || Revision() != revision {
		t.Fatal("rejected reload published snapshot")
	}
	if err := os.WriteFile(path, brainReloadConfig("./data/brain"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); err != nil {
		t.Fatalf("valid unchanged reload: %v", err)
	}
}
