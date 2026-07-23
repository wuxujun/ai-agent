package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRegisterMCPTools_MultipleServersAreNamespacedAndCallable(t *testing.T) {
	first := newMCPTestServer(t, "first-session", "search", "first result")
	defer first.Close()
	second := newMCPTestServer(t, "second-session", "search", "second result")
	defer second.Close()

	registry := NewRegistry()
	manager, summary, err := RegisterMCPTools(context.Background(), []config.MCPServerConfig{
		{Name: "alpha", URL: first.URL, AllowPrivateNetwork: true, RiskLevel: "low"},
		{Name: "beta", URL: second.URL, AllowPrivateNetwork: true},
	}, registry)
	if err != nil {
		t.Fatalf("RegisterMCPTools: %v", err)
	}
	defer manager.Close(context.Background())

	if summary.ServersConfigured != 2 || summary.ServersReady != 2 || summary.ToolsRegistered != 2 || len(summary.Failures) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if got := strings.Join(registry.Names(), ","); got != "mcp_alpha_search,mcp_beta_search" {
		t.Fatalf("registered tools = %s", got)
	}

	alpha, _ := registry.Get("mcp_alpha_search")
	alphaResult, err := alpha.Execute(context.Background(), "", map[string]any{"query": "one"})
	if err != nil {
		t.Fatalf("alpha Execute: %v", err)
	}
	if alphaResult.Observation != "first result" || alpha.RiskLevel() != types.RiskLevelLow {
		t.Fatalf("alpha result/risk = %+v / %s", alphaResult, alpha.RiskLevel())
	}

	beta, _ := registry.Get("mcp_beta_search")
	betaResult, err := beta.Execute(context.Background(), "", map[string]any{"query": "two"})
	if err != nil {
		t.Fatalf("beta Execute: %v", err)
	}
	if betaResult.Observation != "second result" || beta.RiskLevel() != types.RiskLevelHigh {
		t.Fatalf("beta result/risk = %+v / %s", betaResult, beta.RiskLevel())
	}
}

func TestRegisterMCPTools_OptionalFailureIsIsolated(t *testing.T) {
	healthy := newMCPTestServer(t, "healthy-session", "ping", "pong")
	defer healthy.Close()

	registry := NewRegistry()
	manager, summary, err := RegisterMCPTools(context.Background(), []config.MCPServerConfig{
		{Name: "broken", URL: "file:///not-mcp"},
		{Name: "healthy", URL: healthy.URL, AllowPrivateNetwork: true},
	}, registry)
	if err != nil {
		t.Fatalf("optional failure returned error: %v", err)
	}
	defer manager.Close(context.Background())
	if summary.ServersReady != 1 || summary.ToolsRegistered != 1 || summary.Failures["broken"] == "" {
		t.Fatalf("summary = %+v", summary)
	}
	if _, ok := registry.Get("mcp_healthy_ping"); !ok {
		t.Fatal("healthy server tool was not registered")
	}
}

func TestRegisterMCPTools_RequiredFailureReturnsError(t *testing.T) {
	_, summary, err := RegisterMCPTools(context.Background(), []config.MCPServerConfig{
		{Name: "required", URL: "file:///not-mcp", Required: true},
	}, NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "required MCP server") {
		t.Fatalf("error = %v", err)
	}
	if summary.Failures["required"] == "" {
		t.Fatalf("summary = %+v", summary)
	}
}

func newMCPTestServer(t *testing.T, sessionID, toolName, toolResult string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			if request.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("DELETE session = %q, want %q", request.Header.Get("Mcp-Session-Id"), sessionID)
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		method, _ := payload["method"].(string)
		if method != "initialize" && request.Header.Get("Mcp-Session-Id") != sessionID {
			t.Errorf("%s session = %q, want %q", method, request.Header.Get("Mcp-Session-Id"), sessionID)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			writer.Header().Set("Mcp-Session-Id", sessionID)
			fmt.Fprint(writer, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`)
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":%q,"description":"test tool","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]}}`, toolName)
		case "tools/call":
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":%q}]}}`, toolResult)
		default:
			http.Error(writer, "unexpected method", http.StatusBadRequest)
		}
	}))
}
