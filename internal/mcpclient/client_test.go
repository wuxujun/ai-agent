package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestClientDiscoversAndCallsToolsWithOneSession(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	initializeCalls := 0
	closed := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			if request.Header.Get("Mcp-Session-Id") != "session-one" {
				t.Fatalf("close session header = %q", request.Header.Get("Mcp-Session-Id"))
			}
			closed = true
			return response(http.StatusNoContent, "", nil), nil
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		method, _ := payload["method"].(string)
		mu.Lock()
		methods = append(methods, method)
		if method == "initialize" {
			initializeCalls++
		}
		mu.Unlock()

		if method != "initialize" && request.Header.Get("Mcp-Session-Id") != "session-one" {
			t.Fatalf("%s session header = %q", method, request.Header.Get("Mcp-Session-Id"))
		}
		if method != "initialize" && request.Header.Get("MCP-Protocol-Version") != "2024-11-05" {
			t.Fatalf("%s protocol header = %q", method, request.Header.Get("MCP-Protocol-Version"))
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch method {
		case "initialize":
			return response(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`, map[string]string{"Mcp-Session-Id": "session-one"}), nil
		case "notifications/initialized":
			return response(http.StatusAccepted, "", nil), nil
		case "tools/list":
			return response(http.StatusOK, `data: {"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"lookup","description":"Lookup a record","inputSchema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}]}}`+"\n\n", nil), nil
		case "tools/call":
			params := payload["params"].(map[string]any)
			if params["name"] != "lookup" {
				t.Fatalf("remote tool name = %v", params["name"])
			}
			return response(http.StatusOK, `{"jsonrpc":"2.0","id":3,"result":{"structuredContent":{"value":"found"},"content":[]}}`, nil), nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	})

	client, err := NewWithHTTPClient(Config{
		Name:          "one",
		URL:           "https://one.example/mcp",
		Authorization: "secret",
	}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "lookup" || len(tools[0].Required()) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	result, err := client.CallTool(context.Background(), "lookup", map[string]any{"id": "42"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.Text != `{"value":"found"}` {
		t.Fatalf("result text = %q", result.Text)
	}
	if initializeCalls != 1 {
		t.Fatalf("initialize calls = %d, want 1", initializeCalls)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("session was not closed")
	}
	wantMethods := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	if strings.Join(methods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("methods = %v, want %v", methods, wantMethods)
	}
}

func response(status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header)
	for key, value := range headers {
		header.Set(key, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
