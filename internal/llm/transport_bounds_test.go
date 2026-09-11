package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/llmprovider"
)

type boundsTransport func(*http.Request) (*http.Response, error)

func (f boundsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeTrackedBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackedBody) Close() error { b.closed = true; return nil }
func TestStreamBoundsApplyBeforeCallback(t *testing.T) {
	chunk := strings.Repeat("x", 1<<20)
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}}}})
	body := &closeTrackedBody{Reader: strings.NewReader(strings.Repeat("data: "+string(raw)+"\n\n", 6))}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = boundsTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	distributed := 0
	var dest map[string]any
	_, err := (nativeStructuredCaller{}).CallJSONStream(context.Background(), Config{Provider: llmprovider.LiteLLM, BaseURL: "https://fixture.invalid", Model: "fixture"}, "", "", map[string]any{"type": "object"}, &dest, func(s string) { distributed += len(s) })
	if err == nil || !strings.Contains(err.Error(), "4 MiB") || distributed > 4<<20 || !body.closed {
		t.Fatalf("unbounded distribution=%d closed=%v err=%v", distributed, body.closed, err)
	}
}

func TestStrictAdditionalPropertiesSemantics(t *testing.T) {
	for _, tc := range []struct {
		name            string
		additional      any
		present, accept bool
	}{
		{"false", false, true, false}, {"true", true, true, true}, {"omitted", nil, false, true},
		{"schema", map[string]any{"type": "string"}, true, true}, {"invalid_schema", 17, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]any{"type": "object", "properties": map[string]any{"known": map[string]any{"type": "string"}}}
			if tc.present {
				schema["additionalProperties"] = tc.additional
			}
			var dest map[string]any
			err := parseStructuredJSONStrict(`{"known":"ok","extra":"value"}`, &dest, schema)
			if (err == nil) != tc.accept {
				t.Fatalf("accepted=%v want %v err=%v", err == nil, tc.accept, err)
			}
			nested := map[string]any{"type": "object", "properties": map[string]any{"child": schema}, "additionalProperties": false}
			err = parseStructuredJSONStrict(`{"child":{"known":"ok","extra":"value"}}`, &dest, nested)
			if (err == nil) != tc.accept {
				t.Fatalf("nested accepted=%v want %v", err == nil, tc.accept)
			}
		})
	}
	schema := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "number"}}
	var dest map[string]any
	if err := parseStructuredJSONStrict(`{"extra":"bad"}`, &dest, schema); err == nil {
		t.Fatal("additional value schema ignored")
	}
}
