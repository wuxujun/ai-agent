package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
)

const protocolVersion = "2025-11-25"

var supportedProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

var sessionHeaderNames = []string{
	"Mcp-Session-Id",
	"X-Session-Id",
	"Session-Id",
}

type Config struct {
	Name                string
	URL                 string
	Authorization       string
	Timeout             time.Duration
	AllowPrivateNetwork bool
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (t Tool) Properties() map[string]any {
	properties, _ := t.InputSchema["properties"].(map[string]any)
	if properties == nil {
		return map[string]any{}
	}
	return properties
}

func (t Tool) Required() []string {
	raw, _ := t.InputSchema["required"].([]any)
	required := make([]string, 0, len(raw))
	for _, item := range raw {
		if name, ok := item.(string); ok {
			required = append(required, name)
		}
	}
	// Tests and programmatic descriptors often use []string directly.
	if values, ok := t.InputSchema["required"].([]string); ok {
		return append([]string(nil), values...)
	}
	return required
}

type CallResult struct {
	Text              string
	StructuredContent json.RawMessage
}

type Client struct {
	name          string
	endpoint      string
	authorization string
	httpClient    *http.Client

	initializeMu sync.Mutex
	initialized  bool

	headersMu      sync.RWMutex
	sessionHeaders map[string]string
	nextID         atomic.Int64
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Name) == "" {
		return nil, errors.New("mcp client name must not be empty")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if err := policy.ValidateConfiguredURL(config.URL, config.AllowPrivateNetwork); err != nil {
		return nil, fmt.Errorf("mcp server %q endpoint rejected by policy: %w", config.Name, err)
	}
	client := policy.ConfiguredHTTPClient(timeout, config.AllowPrivateNetwork)
	jar, _ := cookiejar.New(nil)
	client.Jar = jar
	return newWithHTTPClient(config, client), nil
}

// NewWithHTTPClient is primarily useful for deterministic protocol tests and
// custom transports. The endpoint is still required, but network policy is the
// responsibility of the supplied transport.
func NewWithHTTPClient(config Config, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(config.Name) == "" {
		return nil, errors.New("mcp client name must not be empty")
	}
	if strings.TrimSpace(config.URL) == "" {
		return nil, errors.New("mcp client url must not be empty")
	}
	if httpClient == nil {
		return nil, errors.New("mcp http client must not be nil")
	}
	return newWithHTTPClient(config, httpClient), nil
}

func newWithHTTPClient(config Config, httpClient *http.Client) *Client {
	return &Client{
		name:           strings.TrimSpace(config.Name),
		endpoint:       strings.TrimSpace(config.URL),
		authorization:  normalizeAuthorization(config.Authorization),
		httpClient:     httpClient,
		sessionHeaders: make(map[string]string),
	}
}

func (c *Client) Name() string { return c.name }

func (c *Client) Initialize(ctx context.Context) error {
	c.initializeMu.Lock()
	defer c.initializeMu.Unlock()
	if c.initialized {
		return nil
	}

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "ai-agent",
			"version": "1.0.0",
		},
	}, &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if !supportedProtocolVersions[result.ProtocolVersion] {
		return fmt.Errorf("initialize: unsupported negotiated protocol version %q", result.ProtocolVersion)
	}
	c.headersMu.Lock()
	c.sessionHeaders["MCP-Protocol-Version"] = result.ProtocolVersion
	c.headersMu.Unlock()
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	c.initialized = true
	return nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	var tools []Tool
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.request(ctx, "tools/list", params, &result); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
	return nil, errors.New("tools/list exceeded 100 pages")
}

func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*CallResult, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	var result struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := c.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	}, &result); err != nil {
		return nil, fmt.Errorf("tools/call %q: %w", name, err)
	}

	parts := make([]string, 0, len(result.Content))
	for _, raw := range result.Content {
		var item struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
			continue
		}
		if len(item.Data) > 0 {
			parts = append(parts, string(item.Data))
			continue
		}
		// Preserve embedded resources, resource links, audio and future content
		// variants as JSON instead of silently discarding them.
		parts = append(parts, string(raw))
	}
	if len(parts) == 0 && len(result.StructuredContent) > 0 {
		parts = append(parts, string(result.StructuredContent))
	}
	text := strings.Join(parts, "\n")
	if result.IsError {
		if strings.TrimSpace(text) == "" {
			text = "remote MCP tool reported an error"
		}
		return nil, errors.New(text)
	}
	return &CallResult{Text: text, StructuredContent: result.StructuredContent}, nil
}

// Close releases a stateful Streamable HTTP session when the server supplied
// a session identifier. Stateless servers need no close request.
func (c *Client) Close(ctx context.Context) error {
	headers := c.headers()
	hasSession := false
	for _, name := range sessionHeaderNames {
		if headers[name] != "" {
			hasSession = true
			break
		}
	}
	if !hasSession {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req, headers)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("close returned status %d", resp.StatusCode)
	}
	return nil
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
	ID json.RawMessage `json:"id"`
}

func (c *Client) request(ctx context.Context, method string, params any, target any) error {
	id := c.nextID.Add(1)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	body, headers, err := c.send(ctx, payload)
	if err != nil {
		return err
	}
	c.captureSessionHeaders(headers)
	if len(body) == 0 {
		return errors.New("empty JSON-RPC response")
	}
	var response rpcResponse
	if err := decodeRPCBody(body, id, &response); err != nil {
		return err
	}
	if response.JSONRPC != "2.0" {
		return errors.New("invalid JSON-RPC response version")
	}
	if response.Error != nil {
		return fmt.Errorf("JSON-RPC error %d: %s", response.Error.Code, response.Error.Message)
	}
	if target != nil && len(response.Result) > 0 {
		if raw, ok := target.(*json.RawMessage); ok {
			*raw = append((*raw)[:0], response.Result...)
			return nil
		}
		if err := json.Unmarshal(response.Result, target); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	_, headers, err := c.send(ctx, payload)
	if err == nil {
		c.captureSessionHeaders(headers)
	}
	return err
}

func (c *Client) send(ctx context.Context, payload any) ([]byte, http.Header, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.applyHeaders(req, c.headers())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, resp.Header, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, fmt.Errorf("POST returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.Header, nil
}

func (c *Client) headers() map[string]string {
	c.headersMu.RLock()
	defer c.headersMu.RUnlock()
	copy := make(map[string]string, len(c.sessionHeaders))
	for key, value := range c.sessionHeaders {
		copy[key] = value
	}
	return copy
}

func (c *Client) captureSessionHeaders(headers http.Header) {
	c.headersMu.Lock()
	defer c.headersMu.Unlock()
	for _, name := range sessionHeaderNames {
		if value := headers.Get(name); value != "" {
			c.sessionHeaders[name] = value
		}
	}
}

func (c *Client) applyHeaders(req *http.Request, sessionHeaders map[string]string) {
	if c.authorization != "" {
		req.Header.Set("Authorization", c.authorization)
	}
	for key, value := range sessionHeaders {
		req.Header.Set(key, value)
	}
}

func decodeRPCBody(body []byte, expectedID int64, target *rpcResponse) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, target); err != nil {
			return fmt.Errorf("decode JSON-RPC response: %w", err)
		}
		if !rpcIDMatches(target.ID, expectedID) {
			return fmt.Errorf("JSON-RPC response id does not match request %d", expectedID)
		}
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 1024), 8<<20)
	var events []string
	var currentData []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if len(currentData) > 0 {
				events = append(events, strings.Join(currentData, "\n"))
				currentData = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			currentData = append(currentData, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(currentData) > 0 {
		events = append(events, strings.Join(currentData, "\n"))
	}
	for _, data := range events {
		if data == "" {
			continue
		}
		var candidate rpcResponse
		if err := json.Unmarshal([]byte(data), &candidate); err == nil && rpcIDMatches(candidate.ID, expectedID) {
			*target = candidate
			return nil
		}
	}
	return errors.New("response contained no valid JSON-RPC object")
}

func rpcIDMatches(raw json.RawMessage, expected int64) bool {
	var numeric int64
	return len(raw) > 0 && json.Unmarshal(raw, &numeric) == nil && numeric == expected
}

func normalizeAuthorization(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, " ") {
		return value
	}
	return "Bearer " + value
}
