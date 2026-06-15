package policy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidateURL_Allows(t *testing.T) {
	originalLookupIP := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupIP = originalLookupIP })

	// Public hosts/IPs that should be permitted.
	allowed := []string{
		"https://example.com",
		"http://example.com/path?q=1",
		"https://1.1.1.1",
		"https://8.8.8.8/resolve",
	}
	for _, u := range allowed {
		if err := ValidateURL(u); err != nil {
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
		"ftp://example.com",  // scheme not allowed
		"file:///etc/passwd", // scheme not allowed
		"justastring",        // no scheme/host
		"",                   // empty
	}
	for _, u := range blocked {
		if err := ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", u)
		}
	}
}

func TestSafeHTTPClientDialsValidatedIP(t *testing.T) {
	originalLookupIP := lookupIP
	originalDial := dialResolvedIP
	lookupCalls := 0
	lookupIP = func(host string) ([]net.IP, error) {
		lookupCalls++
		if lookupCalls == 1 {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	var dialed string
	dialResolvedIP = func(_ context.Context, _, address string, _ time.Duration) (net.Conn, error) {
		dialed = address
		return nil, errors.New("stop after dial capture")
	}
	t.Cleanup(func() {
		lookupIP = originalLookupIP
		dialResolvedIP = originalDial
	})

	client := SafeHTTPClient(time.Second)
	_, _ = client.Get("http://example.com/resource")

	// The transport must perform exactly one lookup and dial that result
	// without resolving the hostname again.
	if lookupCalls != 1 {
		t.Fatalf("lookup calls = %d, want 1", lookupCalls)
	}
	if !strings.HasPrefix(dialed, "93.184.216.34:") {
		t.Fatalf("dialed address = %q, want validated public IP", dialed)
	}
}

func TestSafeHTTPClientBlocksPrivateDialTarget(t *testing.T) {
	originalLookupIP := lookupIP
	originalDial := dialResolvedIP
	lookupIP = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	dialCalled := false
	dialResolvedIP = func(_ context.Context, _, _ string, _ time.Duration) (net.Conn, error) {
		dialCalled = true
		return nil, errors.New("unexpected dial")
	}
	t.Cleanup(func() {
		lookupIP = originalLookupIP
		dialResolvedIP = originalDial
	})

	client := SafeHTTPClient(time.Second)
	_, err := client.Get("http://example.com/resource")
	if err == nil || !strings.Contains(err.Error(), "private/restricted IP blocked") {
		t.Fatalf("SafeHTTPClient error = %v, want private IP rejection", err)
	}
	if dialCalled {
		t.Fatal("dial must not run for a blocked IP")
	}
}

func TestSafeHTTPClientBlocksPrivateRedirect(t *testing.T) {
	client := SafeHTTPClient(time.Second)
	req := &http.Request{URL: &url.URL{Scheme: "http", Host: "127.0.0.1", Path: "/metadata"}}
	err := client.CheckRedirect(req, []*http.Request{{URL: &url.URL{Scheme: "https", Host: "example.com"}}})
	if err == nil || !strings.Contains(err.Error(), "redirect violation") {
		t.Fatalf("CheckRedirect error = %v, want SSRF redirect rejection", err)
	}
}
