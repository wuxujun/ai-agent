package planner

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// mkResp builds a minimal *http.Response wrapping body for parser tests.
func mkResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func mkStreamResp(body string) *http.Response {
	response := mkResp(body)
	response.Header = http.Header{"Content-Type": []string{"text/event-stream"}}
	return response
}

// validResponsesBody returns an OpenAI Responses-API payload carrying the given
// usage block, with a well-formed output[].content[].text so extractStructuredText
// succeeds.
func validResponsesBody(usage string) string {
	return `{
		"output": [
			{"content": [{"type": "output_text", "text": "{\"stop\":true}"}]}
		],
		"usage": ` + usage + `
	}`
}

// TestParseOpenAIResponsesReadsResponsesUsageFields is the regression test for
// BUG_REPORT.md #9: the default provider parsed Chat-Completions usage field
// names (prompt_tokens/completion_tokens), but the Responses API reports
// input_tokens/output_tokens, so token usage was always 0.
func TestParseOpenAIResponsesReadsResponsesUsageFields(t *testing.T) {
	body := validResponsesBody(`{"input_tokens": 120, "output_tokens": 45, "total_tokens": 165}`)

	_, usage, err := parseOpenAIResponses(mkResp(body), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if usage.PromptTokens != 120 {
		t.Errorf("PromptTokens = %d, want 120 (from input_tokens)", usage.PromptTokens)
	}
	if usage.CompletionTokens != 45 {
		t.Errorf("CompletionTokens = %d, want 45 (from output_tokens)", usage.CompletionTokens)
	}
	if usage.TotalTokens != 165 {
		t.Errorf("TotalTokens = %d, want 165", usage.TotalTokens)
	}
}

// TestParseOpenAIResponsesLegacyChatFields ensures the legacy Chat-Completions
// field names still parse (back-compat fallback).
func TestParseOpenAIResponsesLegacyChatFields(t *testing.T) {
	body := validResponsesBody(`{"prompt_tokens": 10, "completion_tokens": 7, "total_tokens": 17}`)

	_, usage, err := parseOpenAIResponses(mkResp(body), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 7 || usage.TotalTokens != 17 {
		t.Errorf("usage = %+v, want {10 7 17} from legacy chat fields", usage)
	}
}

// TestParseOpenAIResponsesDerivesTotalWhenMissing verifies total_tokens is
// computed from input+output when the payload omits it.
func TestParseOpenAIResponsesDerivesTotalWhenMissing(t *testing.T) {
	body := validResponsesBody(`{"input_tokens": 100, "output_tokens": 30}`)

	_, usage, err := parseOpenAIResponses(mkResp(body), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if usage.TotalTokens != 130 {
		t.Errorf("TotalTokens = %d, want 130 (derived from input+output)", usage.TotalTokens)
	}
}

func TestParseOpenAIResponsesStreamsDeltasAndUsage(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"stop\\\":\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"true}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":3,\"total_tokens\":15}}}\n\n" +
		"data: [DONE]\n\n"
	var chunks []string
	text, usage, err := parseOpenAIResponses(mkStreamResp(body), func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"stop":true}` {
		t.Fatalf("text = %q", text)
	}
	if len(chunks) != 2 || strings.Join(chunks, "") != text {
		t.Fatalf("chunks = %#v", chunks)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseOpenAIChatReadsUsageAfterDoneMarker(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"thought_summary\\\":\\\"done\\\"}\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3308,\"completion_tokens\":931,\"total_tokens\":4239}}\n\n"

	text, usage, err := parseOpenAIChat(mkResp(body), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if text != `{"thought_summary":"done"}` {
		t.Fatalf("text = %q", text)
	}
	if usage.PromptTokens != 3308 || usage.CompletionTokens != 931 || usage.TotalTokens != 4239 {
		t.Fatalf("usage = %+v, want {3308 931 4239}", usage)
	}
}
