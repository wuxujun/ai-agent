package multiagent

import (
	"testing"
)

// These tests exercise the provider response parsers directly with fixed JSON
// fixtures, so they need no network or real LLM. They cover the previously
// untested extract*Text / parseJSONInto functions in llm.go.

func TestExtractResponsesText(t *testing.T) {
	raw := []byte(`{
		"output": [
			{"content": [
				{"type": "output_text", "text": "{\"final_answer\":\"hi\"}"}
			]}
		],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)

	text, usage, err := extractResponsesText(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != `{"final_answer":"hi"}` {
		t.Errorf("unexpected text: %q", text)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("unexpected usage: %+v", usage)
	}
}

func TestExtractResponsesText_MissingOutput(t *testing.T) {
	if _, _, err := extractResponsesText([]byte(`{"usage":{}}`)); err == nil {
		t.Error("expected error when output field is missing")
	}
}

func TestExtractResponsesText_TextNotFound(t *testing.T) {
	// output present but no text part inside content.
	raw := []byte(`{"output":[{"content":[{"type":"output_text"}]}]}`)
	if _, _, err := extractResponsesText(raw); err == nil {
		t.Error("expected error when no text part is found")
	}
}

func TestExtractResponsesText_InvalidJSON(t *testing.T) {
	if _, _, err := extractResponsesText([]byte(`not json`)); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestExtractChatText(t *testing.T) {
	raw := []byte(`{
		"choices": [{"message": {"content": "hello world"}}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}
	}`)

	text, usage, err := extractChatText(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hello world" {
		t.Errorf("unexpected text: %q", text)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 4 || usage.TotalTokens != 7 {
		t.Errorf("unexpected usage: %+v", usage)
	}
}

func TestExtractChatText_EmptyChoices(t *testing.T) {
	if _, _, err := extractChatText([]byte(`{"choices":[]}`)); err == nil {
		t.Error("expected error when choices are empty")
	}
}

func TestExtractOllamaText(t *testing.T) {
	raw := []byte(`{
		"message": {"content": "the answer"},
		"prompt_eval_count": 8,
		"eval_count": 2
	}`)

	text, usage, err := extractOllamaText(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "the answer" {
		t.Errorf("unexpected text: %q", text)
	}
	// Ollama derives total from prompt_eval_count + eval_count.
	if usage.PromptTokens != 8 || usage.CompletionTokens != 2 || usage.TotalTokens != 10 {
		t.Errorf("unexpected usage: %+v", usage)
	}
}

func TestExtractOllamaText_InvalidJSON(t *testing.T) {
	if _, _, err := extractOllamaText([]byte(`{`)); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestParseJSONInto_Direct(t *testing.T) {
	var dest struct {
		Answer string `json:"answer"`
	}
	if err := parseJSONInto(`{"answer":"42"}`, &dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest.Answer != "42" {
		t.Errorf("unexpected answer: %q", dest.Answer)
	}
}

func TestParseJSONInto_FencedFallback(t *testing.T) {
	// LLMs frequently wrap JSON in markdown fences or prose; the fallback path
	// must extract the outermost {...} object.
	var dest struct {
		Answer string `json:"answer"`
	}
	input := "Here is the result:\n```json\n{\"answer\":\"ok\"}\n```\nDone."
	if err := parseJSONInto(input, &dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest.Answer != "ok" {
		t.Errorf("unexpected answer: %q", dest.Answer)
	}
}

func TestParseJSONInto_Unparseable(t *testing.T) {
	var dest struct{}
	if err := parseJSONInto("no braces here", &dest); err == nil {
		t.Error("expected error when no JSON object is present")
	}
}
