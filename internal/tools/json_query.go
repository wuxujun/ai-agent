package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

const maxJSONQueryInputBytes = 2 << 20

// JSONQueryTool selects a value from a workspace JSON file with an RFC 6901
// JSON Pointer. It is intentionally read-only and has no external jq/runtime
// dependency.
type JSONQueryTool struct{}

func (t *JSONQueryTool) Name() string { return "json_query" }

func (t *JSONQueryTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }

func (t *JSONQueryTool) Description() string {
	return "Read a workspace JSON file and select a value using an RFC 6901 JSON Pointer"
}

func (t *JSONQueryTool) Parameters() map[string]any {
	return map[string]any{
		"path":    map[string]any{"type": "string", "description": "Workspace-relative JSON file path"},
		"pointer": map[string]any{"type": "string", "description": "RFC 6901 JSON Pointer, for example /users/0/name; empty selects the root"},
	}
}

func (t *JSONQueryTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("json_query requires non-empty path")
	}
	if filepath.IsAbs(path) || strings.Contains(path, "..") {
		return fmt.Errorf("invalid json_query path")
	}
	pointer, _ := params["pointer"].(string)
	if pointer != "" && !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("json_query pointer must be empty or start with /")
	}
	return nil
}

func (t *JSONQueryTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	if err := t.Validate(params); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(params["path"].(string))
	pointer, _ := params["pointer"].(string)
	fullPath := filepath.Join(workspace, path)
	if err := policy.ValidateReadPath(workspace, fullPath); err != nil {
		return nil, fmt.Errorf("json_query policy violation: %w", err)
	}
	f, err := os.OpenFile(fullPath, os.O_RDONLY|policy.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open JSON file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat JSON file: %w", err)
	}
	if info.Size() > maxJSONQueryInputBytes {
		return nil, fmt.Errorf("JSON file exceeds %d byte limit", maxJSONQueryInputBytes)
	}

	decoder := json.NewDecoder(io.LimitReader(f, maxJSONQueryInputBytes+1))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode JSON file: %w", err)
	}
	if decoder.InputOffset() > maxJSONQueryInputBytes {
		return nil, fmt.Errorf("JSON file exceeds %d byte limit", maxJSONQueryInputBytes)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}

	selected, err := resolveJSONPointer(document, pointer)
	if err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(selected, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode selected JSON value: %w", err)
	}
	return &ToolResult{
		Query:       pointer,
		Observation: string(encoded),
		Evidence: []types.Evidence{{
			Path:  path,
			Lines: strings.Split(string(encoded), "\n"),
			Query: pointer,
		}},
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing JSON data: %w", err)
	}
	return fmt.Errorf("JSON file contains multiple top-level values")
}

func resolveJSONPointer(document any, pointer string) (any, error) {
	if pointer == "" {
		return document, nil
	}
	current := document
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token, err := decodeJSONPointerToken(rawToken)
		if err != nil {
			return nil, fmt.Errorf("JSON pointer %q: %w", pointer, err)
		}
		switch value := current.(type) {
		case map[string]any:
			next, ok := value[token]
			if !ok {
				return nil, fmt.Errorf("JSON pointer %q: object key %q not found", pointer, token)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("JSON pointer %q: invalid array index %q", pointer, token)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("JSON pointer %q: cannot descend through scalar at %q", pointer, token)
		}
	}
	return current, nil
}

func decodeJSONPointerToken(raw string) (string, error) {
	var decoded strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '~' {
			decoded.WriteByte(raw[i])
			continue
		}
		if i+1 >= len(raw) || (raw[i+1] != '0' && raw[i+1] != '1') {
			return "", fmt.Errorf("invalid escape in token %q", raw)
		}
		i++
		if raw[i] == '0' {
			decoded.WriteByte('~')
		} else {
			decoded.WriteByte('/')
		}
	}
	return decoded.String(), nil
}

func init() {
	Register(&JSONQueryTool{})
}
