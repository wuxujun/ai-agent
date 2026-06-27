package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"

	"github.com/wuxujun/ai-agent/internal/types"
)

var ragHTTPClient = func() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
	}
}()

// SearchThirdPartyRAG queries an external/third-party RAG URL to retrieve relevant historical memories/knowledge.
func SearchThirdPartyRAG(ctx context.Context, query string) ([]types.Memory, error) {
	cfg := config.Get()
	ragURL := cfg.RAG.SearchURL
	if ragURL == "" {
		return nil, nil // No third-party RAG URL configured
	}

	method := cfg.RAG.SearchMethod
	if method == "" {
		method = "GET"
	}

	log.Info("Querying third-party RAG", "url", ragURL, "query", query, "method", method)

	// First attempt: try sending the request as configured
	mems, err := doRAGRequest(ctx, ragURL, method, query, cfg.RAG.Authorization, false)
	if err != nil {
		errStr := err.Error()
		// Self-healing: if the endpoint is an MCP server and rejected our payload
		// with an invalid message version / expected 2.0 error, or session init error, retry as JSON-RPC 2.0 with handshake.
		if strings.Contains(errStr, "invalid message version tag") ||
			strings.Contains(errStr, "expected \"2.0\"") ||
			strings.Contains(errStr, "malformed payload") ||
			strings.Contains(errStr, "invalid during session initialization") ||
			strings.Contains(errStr, "session") ||
			strings.Contains(errStr, "jsonrpc") {
			log.Warn("Detected MCP server requiring initialization, launching MCP client query", "url", ragURL)
			return queryMCP(ctx, ragURL, cfg.RAG.Authorization, query)
		}
		return nil, err
	}

	return mems, nil
}

func doRAGRequest(ctx context.Context, ragURL, method, query, auth string, forceJSONRPC bool) ([]types.Memory, error) {
	var req *http.Request
	var err error

	upperMethod := strings.ToUpper(method)
	isJSONRPC := forceJSONRPC || upperMethod == "MCP" || upperMethod == "JSON-RPC" || (upperMethod == "POST" && strings.Contains(ragURL, "/tools/call"))

	if isJSONRPC {
		cfg := config.Get()
		toolName := cfg.RAG.ToolName
		if toolName == "" {
			toolName = "search"
		}
		payload := map[string]any{
			"jsonrpc": "2.0",
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": buildSearchArguments(query),
			},
			"id": 1,
		}
		jsonBytes, marErr := json.Marshal(payload)
		if marErr != nil {
			return nil, fmt.Errorf("failed to marshal JSON-RPC request: %w", marErr)
		}
		req, err = http.NewRequestWithContext(ctx, "POST", ragURL, bytes.NewBuffer(jsonBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create JSON-RPC POST request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else if upperMethod == "POST" {
		body := map[string]string{"query": query}
		jsonBytes, marErr := json.Marshal(body)
		if marErr != nil {
			return nil, fmt.Errorf("failed to marshal query to JSON: %w", marErr)
		}
		req, err = http.NewRequestWithContext(ctx, "POST", ragURL, bytes.NewBuffer(jsonBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create POST request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		// GET
		parsedURL, parseErr := url.Parse(ragURL)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid third-party RAG URL: %w", parseErr)
		}
		q := parsedURL.Query()
		q.Set("q", query)
		q.Set("query", query)
		parsedURL.RawQuery = q.Encode()

		req, err = http.NewRequestWithContext(ctx, "GET", parsedURL.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create GET request: %w", err)
		}
	}

	req.Header.Set("Accept", "application/json, text/event-stream")

	if auth != "" {
		authVal := auth
		if !strings.HasPrefix(strings.ToLower(authVal), "bearer ") {
			authVal = "Bearer " + authVal
		}
		req.Header.Set("Authorization", authVal)
	}

	resp, err := ragHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to third-party RAG failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("third-party RAG returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return parseRAGResponse(bodyBytes)
}

func parseRAGResponse(data []byte) ([]types.Memory, error) {
	dataStr := string(data)
	if strings.Contains(dataStr, "data:") {
		lines := strings.Split(dataStr, "\n")
		var jsonData string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				jsonData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				break
			}
		}
		if jsonData != "" {
			data = []byte(jsonData)
		}
	}
	// Try parsing as JSON-RPC 2.0 response first
	var rpcResp struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &rpcResp); err == nil && rpcResp.JSONRPC == "2.0" {
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("MCP JSON-RPC error: %s (code %d)", rpcResp.Error.Message, rpcResp.Error.Code)
		}
		if rpcResp.Result.IsError {
			var errMsg []string
			for _, content := range rpcResp.Result.Content {
				if content.Type == "text" && content.Text != "" {
					errMsg = append(errMsg, content.Text)
				}
			}
			if len(errMsg) > 0 {
				return nil, fmt.Errorf("MCP tool execution error: %s", strings.Join(errMsg, "; "))
			}
			return nil, fmt.Errorf("MCP tool execution failed (isError: true)")
		}
		var allMemories []types.Memory
		for _, content := range rpcResp.Result.Content {
			if content.Type == "text" && content.Text != "" {
				// Try to parse the text as memories JSON
				if mems, parseErr := parseRAGResponse([]byte(content.Text)); parseErr == nil && len(mems) > 0 {
					allMemories = append(allMemories, mems...)
				} else {
					// Fallback: treat the raw text as key findings of a single memory
					allMemories = append(allMemories, types.Memory{
						ID:          fmt.Sprintf("mem-ext-%d", len(allMemories)+1),
						KeyFindings: content.Text,
					})
				}
			}
		}
		if len(allMemories) > 0 {
			return allMemories, nil
		}
	}

	// Try parsing as simple array of Memory objects first
	var list []types.Memory
	if err := json.Unmarshal(data, &list); err == nil && len(list) > 0 {
		return list, nil
	}

	// Try parsing as a map of slices, extracting standard fields dynamically
	var rawMap map[string]any
	if err := json.Unmarshal(data, &rawMap); err == nil {
		// Look for common keys containing lists of matches/memories
		for _, key := range []string{"results", "memories", "items", "documents", "data", "matches"} {
			if val, ok := rawMap[key]; ok {
				if sliceVal, isSlice := val.([]any); isSlice {
					return parseSliceToMemories(sliceVal), nil
				}
			}
		}
	}

	// Also support parsing raw slice of generic maps if it was parsed as []any directly
	var rawSlice []any
	if err := json.Unmarshal(data, &rawSlice); err == nil {
		return parseSliceToMemories(rawSlice), nil
	}

	return nil, fmt.Errorf("unable to parse third-party RAG response")
}

func parseSliceToMemories(slice []any) []types.Memory {
	var memories []types.Memory
	for _, item := range slice {
		itemMap, isMap := item.(map[string]any)
		if !isMap {
			continue
		}

		mem := types.Memory{}

		// 1. Extract ID
		if id, ok := getStringField(itemMap, "id", "uuid"); ok {
			mem.ID = id
		} else {
			mem.ID = fmt.Sprintf("mem-ext-%d", len(memories)+1)
		}

		// 2. Extract Goal
		if goal, ok := getStringField(itemMap, "goal", "title", "query", "task"); ok {
			mem.Goal = goal
		}

		// 3. Extract Key Findings
		if findings, ok := getStringField(itemMap, "key_findings", "findings", "content", "text", "snippet", "body", "description"); ok {
			mem.KeyFindings = findings
		}

		// 4. Extract Final Answer
		if answer, ok := getStringField(itemMap, "final_answer", "answer", "result", "output"); ok {
			mem.FinalAnswer = answer
		}

		memories = append(memories, mem)
	}
	return memories
}

func getStringField(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			if strVal, isStr := val.(string); isStr {
				return strVal, true
			}
		}
	}
	return "", false
}

func queryMCP(ctx context.Context, ragURL, auth, query string) ([]types.Memory, error) {
	log.Info("Attempting MCP SSE handshake", "url", ragURL)

	// Create SSE GET request
	req, err := http.NewRequestWithContext(ctx, "GET", ragURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if auth != "" {
		authVal := auth
		if !strings.HasPrefix(strings.ToLower(authVal), "bearer ") {
			authVal = "Bearer " + authVal
		}
		req.Header.Set("Authorization", authVal)
	}

	resp, err := ragHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Capture initial session headers from GET response (e.g. Mcp-Session-Id, Cookie etc. are handled by CookieJar)
	initialHeaders := make(map[string]string)
	for _, key := range []string{"Mcp-Session-Id", "X-Session-Id", "Session-Id", "mcp-session-id", "x-session-id", "session-id"} {
		if val := resp.Header.Get(key); val != "" {
			initialHeaders[key] = val
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var endpointURL string
		reader := bufio.NewReader(resp.Body)
		var lastEvent string

		// Set a timeout for reading the endpoint event to avoid hanging forever
		readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
		defer readCancel()

		errChan := make(chan error, 1)
		go func() {
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					errChan <- readErr
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "event:") {
					lastEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				} else if strings.HasPrefix(line, "data:") {
					dataVal := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if lastEvent == "endpoint" {
						endpointURL = dataVal
						errChan <- nil
						return
					}
				}
			}
		}()

		select {
		case <-readCtx.Done():
			log.Warn("Timeout waiting for SSE endpoint event, falling back to direct POST")
		case readErr := <-errChan:
			if readErr == nil && endpointURL != "" {
				base, parseErr := url.Parse(ragURL)
				if parseErr == nil {
					resolvedURL, parseErr := base.Parse(endpointURL)
					if parseErr == nil {
						postURL := resolvedURL.String()
						log.Info("MCP SSE endpoint resolved", "post_url", postURL)
						return doMCPHandshakeAndCall(ctx, postURL, auth, query, initialHeaders)
					}
				}
			}
		}
	}

	// Fallback/Direct: if it wasn't an SSE endpoint or SSE connection failed/timed out,
	// run the 3-step initialization flow directly on ragURL.
	log.Info("Running direct MCP initialization on URL", "url", ragURL)
	return doMCPHandshakeAndCall(ctx, ragURL, auth, query, initialHeaders)
}

func doMCPHandshakeAndCall(ctx context.Context, postURL, auth, query string, initialHeaders map[string]string) ([]types.Memory, error) {
	authVal := auth
	if authVal != "" && !strings.HasPrefix(strings.ToLower(authVal), "bearer ") {
		authVal = "Bearer " + authVal
	}

	// Create sessionHeaders copy
	sessionHeaders := make(map[string]string)
	for k, v := range initialHeaders {
		sessionHeaders[k] = v
	}

	// Step 1: initialize
	initPayload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "ai-agent",
				"version": "1.0.0",
			},
		},
		"id": 1,
	}
	var initResp any
	respHeaders, err := sendJSONRPCWithHeaders(ctx, postURL, authVal, initPayload, &initResp, sessionHeaders)
	if err != nil {
		return nil, fmt.Errorf("MCP initialize failed: %w", err)
	}

	// Propagate session headers returned by the server
	for _, key := range []string{"Mcp-Session-Id", "X-Session-Id", "Session-Id", "mcp-session-id", "x-session-id", "session-id"} {
		if val := respHeaders.Get(key); val != "" {
			sessionHeaders[key] = val
		}
	}

	// Step 2: initialized notification
	initializedPayload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	_, err = sendJSONRPCWithHeaders(ctx, postURL, authVal, initializedPayload, nil, sessionHeaders)
	if err != nil {
		return nil, fmt.Errorf("MCP initialized notification failed: %w", err)
	}

	// Step 3: tools/call search
	cfg := config.Get()
	toolName := cfg.RAG.ToolName
	if toolName == "" {
		toolName = "search"
	}
	toolPayload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": buildSearchArguments(query),
		},
		"id": 2,
	}
	var rpcResp struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_, err = sendJSONRPCWithHeaders(ctx, postURL, authVal, toolPayload, &rpcResp, sessionHeaders)
	if err != nil {
		return nil, fmt.Errorf("MCP tools/call failed: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP JSON-RPC error: %s (code %d)", rpcResp.Error.Message, rpcResp.Error.Code)
	}
	if rpcResp.Result.IsError {
		var errMsg []string
		for _, content := range rpcResp.Result.Content {
			if content.Type == "text" && content.Text != "" {
				errMsg = append(errMsg, content.Text)
			}
		}
		if len(errMsg) > 0 {
			return nil, fmt.Errorf("MCP tool execution error: %s", strings.Join(errMsg, "; "))
		}
		return nil, fmt.Errorf("MCP tool execution failed (isError: true)")
	}

	var allMemories []types.Memory
	for _, content := range rpcResp.Result.Content {
		if content.Type == "text" && content.Text != "" {
			if mems, parseErr := parseRAGResponse([]byte(content.Text)); parseErr == nil && len(mems) > 0 {
				allMemories = append(allMemories, mems...)
			} else {
				allMemories = append(allMemories, types.Memory{
					ID:          fmt.Sprintf("mem-ext-%d", len(allMemories)+1),
					KeyFindings: content.Text,
				})
			}
		}
	}

	return allMemories, nil
}

func sendJSONRPCWithHeaders(ctx context.Context, postURL, auth string, payload any, respTarget any, extraHeaders map[string]string) (http.Header, error) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", postURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := ragHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse SSE-wrapped JSON-RPC responses (e.g. data: {"jsonrpc": "2.0", ...})
	bodyStr := string(bodyBytes)
	if strings.Contains(bodyStr, "data:") {
		lines := strings.Split(bodyStr, "\n")
		var jsonData string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				jsonData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				break
			}
		}
		if jsonData != "" {
			bodyBytes = []byte(jsonData)
		}
	}

	if respTarget != nil {
		if err := json.Unmarshal(bodyBytes, respTarget); err != nil {
			return nil, err
		}
	}
	return resp.Header, nil
}

func buildSearchArguments(query string) map[string]any {
	return map[string]any{
		"query":           query,
		"top_k":           5,
		"min_score":       0.0,
		"rewrite":         false,
		"include_sources": true,
		"include_chunks":  true,
	}
}
