package policy_test

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/policy"
)

func TestValidateURL_Allows(t *testing.T) {
	// Public hosts/IPs that should be permitted.
	allowed := []string{
		"https://example.com",
		"http://example.com/path?q=1",
		"https://1.1.1.1",
		"https://8.8.8.8/resolve",
	}
	for _, u := range allowed {
		if err := policy.ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateURL_Blocks(t *testing.T) {
	blocked := []string{
		"http://localhost:8080",
		"http://127.0.0.1",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.5",
		"http://192.168.1.10",
		"http://172.16.0.1",
		"http://100.64.0.1", // carrier-grade NAT
		"http://0.0.0.0",
		"ftp://example.com",      // scheme not allowed
		"file:///etc/passwd",     // scheme not allowed
		"justastring",            // no scheme/host
		"",                       // empty
	}
	for _, u := range blocked {
		if err := policy.ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", u)
		}
	}
}
