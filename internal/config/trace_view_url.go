package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const traceIDPlaceholder = "{trace_id}"

func validateTraceViewURLTemplate(template string) error {
	if template == "" {
		return nil
	}
	invalid := func() error {
		return fmt.Errorf("api.trace_view_url_template must be an absolute HTTPS URL with one {trace_id} placeholder (HTTP loopback is allowed)")
	}
	if len(template) > 2048 || strings.TrimSpace(template) != template || strings.Contains(template, `\`) || strings.Count(template, traceIDPlaceholder) != 1 {
		return invalid()
	}
	parsed, err := url.Parse(template)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(parsed.Host, traceIDPlaceholder) {
		return invalid()
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" {
		return invalid()
	}
	host := parsed.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return invalid()
		}
	}
	return nil
}

// TraceViewURL returns an operator-configured viewer link for a valid OTel
// trace ID. Empty or invalid inputs produce no link.
func TraceViewURL(template, traceID string) string {
	if template == "" || validateTraceViewURLTemplate(template) != nil || len(traceID) != 32 {
		return ""
	}
	decoded, err := hex.DecodeString(traceID)
	if err != nil {
		return ""
	}
	nonzero := false
	for _, b := range decoded {
		if b != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		return ""
	}
	return strings.Replace(template, traceIDPlaceholder, strings.ToLower(traceID), 1)
}
