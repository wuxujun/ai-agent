package llm

import "testing"

func TestExtractStructuredResponses(t *testing.T) {
	text, usage, err := extractStructuredResponse("responses", []byte(`{"output":[{"content":[{"text":"{\"answer\":\"ok\"}"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
	if err != nil || text != `{"answer":"ok"}` || usage.TotalTokens != 15 {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestExtractStructuredChatAndOllama(t *testing.T) {
	text, usage, err := extractStructuredResponse("chat", []byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if err != nil || text != "hello" || usage.TotalTokens != 7 {
		t.Fatalf("chat text=%q usage=%+v err=%v", text, usage, err)
	}
	text, usage, err = extractStructuredResponse("ollama", []byte(`{"message":{"content":"answer"},"prompt_eval_count":8,"eval_count":2}`))
	if err != nil || text != "answer" || usage.TotalTokens != 10 {
		t.Fatalf("ollama text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestParseStructuredJSONFallback(t *testing.T) {
	var output struct {
		Answer string `json:"answer"`
	}
	if err := parseStructuredJSON("result: ```json\n{\"answer\":\"42\"}\n```", &output); err != nil || output.Answer != "42" {
		t.Fatalf("output=%+v err=%v", output, err)
	}
	if err := parseStructuredJSON("no object", &output); err == nil {
		t.Fatal("expected unparseable response error")
	}
}
