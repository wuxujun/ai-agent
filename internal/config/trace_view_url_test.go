package config

import (
	"os"
	"strings"
	"testing"
)

func TestTraceViewURLTemplate_ValidationAndRendering(t *testing.T) {
	const traceID = "0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name, template string
		valid          bool
	}{
		{"disabled", "", true},
		{"https path", "https://viewer.example/trace/{trace_id}", true},
		{"https query", "https://viewer.example/explore?trace={trace_id}&source=console", true},
		{"http loopback", "http://127.0.0.1:16686/trace/{trace_id}", true},
		{"http remote", "http://viewer.example/trace/{trace_id}", false},
		{"script", "javascript:alert({trace_id})", false},
		{"missing placeholder", "https://viewer.example/trace", false},
		{"duplicate placeholder", "https://viewer.example/{trace_id}/{trace_id}", false},
		{"dynamic host", "https://{trace_id}.example/trace", false},
		{"credentials", "https://user:pass@viewer.example/{trace_id}", false},
		{"fragment", "https://viewer.example/{trace_id}#token", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateTraceViewURLTemplate(test.template)
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, want valid %t", err, test.valid)
			}
			url := TraceViewURL(test.template, traceID)
			if test.valid && test.template != "" && !strings.Contains(url, traceID) {
				t.Fatalf("rendered URL = %q", url)
			}
			if (!test.valid || test.template == "") && url != "" {
				t.Fatalf("unexpected URL = %q", url)
			}
		})
	}
	if url := TraceViewURL("https://viewer.example/trace/{trace_id}", strings.Repeat("0", 32)); url != "" {
		t.Fatalf("zero Trace ID accepted: %q", url)
	}
	if url := TraceViewURL("https://viewer.example/trace/{trace_id}", "invalid"); url != "" {
		t.Fatalf("invalid Trace ID accepted: %q", url)
	}
}

func TestReloadTraceViewURLTemplate_ValidThenRejectedPreservesSnapshot(t *testing.T) {
	configPath, before := loadBrainReloadFixture(t)
	const validTemplate = "https://viewer.example/trace/{trace_id}?source=console"
	validConfig := append(brainReloadConfig("./data/brain"), []byte("api:\n  trace_view_url_template: \""+validTemplate+"\"\n")...)
	if err := os.WriteFile(configPath, validConfig, 0644); err != nil {
		t.Fatal(err)
	}
	active, changes, err := Reload()
	if err != nil || active == before || active.API.TraceViewURLTemplate != validTemplate {
		t.Fatalf("valid reload = %+v, %v", active, err)
	}
	for _, change := range changes {
		if strings.Contains(change, "source=console") {
			t.Fatalf("trace viewer URL leaked to config change log: %q", change)
		}
	}
	invalidConfig := append(brainReloadConfig("./data/brain"), []byte("api:\n  trace_view_url_template: \"javascript:alert({trace_id})\"\n")...)
	if err := os.WriteFile(configPath, invalidConfig, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); err == nil || !strings.Contains(err.Error(), "api.trace_view_url_template") {
		t.Fatalf("invalid reload = %v", err)
	}
	if Get() != active {
		t.Fatal("invalid trace viewer reload replaced active config")
	}
}

func TestTraceViewURLTemplate_EnvironmentOverride(t *testing.T) {
	const template = "https://viewer.example/trace/{trace_id}"
	t.Setenv("AI_AGENT_API_TRACE_VIEW_URL_TEMPLATE", template)
	resetConfig()
	t.Cleanup(resetConfig)
	setupViper()
	cfg, err := unmarshalConfig()
	if err != nil || cfg.API.TraceViewURLTemplate != template {
		t.Fatalf("environment override = %q, %v", cfg.API.TraceViewURLTemplate, err)
	}
}
