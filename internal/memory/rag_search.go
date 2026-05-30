package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// SearchThirdPartyRAG queries an external/third-party RAG URL to retrieve relevant historical memories/knowledge.
func SearchThirdPartyRAG(ctx context.Context, query string) ([]types.Memory, error) {
	ragURL := os.Getenv("AI_AGENT_RAG_SEARCH_URL")
	if ragURL == "" {
		return nil, nil // No third-party RAG URL configured
	}

	method := os.Getenv("AI_AGENT_RAG_SEARCH_METHOD")
	if method == "" {
		method = "GET"
	}

	log.Printf("[RAG/External] Querying third-party URL %s for query %q (method: %s)", ragURL, query, method)

	var req *http.Request
	var err error

	client := &http.Client{Timeout: 10 * time.Second}

	if strings.ToUpper(method) == "POST" {
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

	resp, err := client.Do(req)
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
