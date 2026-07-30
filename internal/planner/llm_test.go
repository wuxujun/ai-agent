package planner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeHTTPClient(status int, contentType, body string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{contentType}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
}

func TestLLMPlannerProviders(t *testing.T) {
	// Sample task
	task := &types.Task{
		ID:         "test-task-1",
		Goal:       "find hello file",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
	}

	expectedDecision := PlanDecision{
		ThoughtSummary: "Finding files",
		Stop:           false,
		FinalAnswer:    "",
		Actions: []ActionCall{
			{
				Action: "find_files",
				Parameters: map[string]any{
					"pattern": "*",
				},
			},
		},
	}

	expectedJSONBytes, _ := json.Marshal(expectedDecision)
	expectedJSONStr := string(expectedJSONBytes)

	t.Run("openai-responses", func(t *testing.T) {
		// Mock OpenAI responses API format
		resp := map[string]any{
			"output": []any{
				map[string]any{
					"content": []any{
						map[string]any{
							"text": expectedJSONStr,
						},
					},
				},
			},
		}
		b, _ := json.Marshal(resp)

		planner := NewLLMPlannerWithProvider(ProviderOpenAIResponses, "test-key", "gpt-4.1", "https://llm.test/responses")
		planner.Client = fakeHTTPClient(http.StatusOK, "application/json", string(b))
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("openai", func(t *testing.T) {
		// Mock OpenAI chat completions format
		resp := map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"content": expectedJSONStr,
					},
				},
			},
		}
		b, _ := json.Marshal(resp)
		body := "data: " + string(b) + "\n\n" + "data: [DONE]\n\n"

		planner := NewLLMPlannerWithProvider(ProviderOpenAI, "test-key", "gpt-4o", "https://llm.test/chat/completions")
		planner.Client = fakeHTTPClient(http.StatusOK, "text/event-stream", body)
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("ollama", func(t *testing.T) {
		// Mock Ollama chat response format
		resp := map[string]any{
			"message": map[string]any{
				"content": expectedJSONStr,
			},
			"done": true,
		}
		b, _ := json.Marshal(resp)

		planner := NewLLMPlannerWithProvider(ProviderOllama, "", "llama3", "https://ollama.test/api/chat")
		planner.Client = fakeHTTPClient(http.StatusOK, "application/x-ndjson", string(b)+"\n")
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		// Mock Gemini SDK response format
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"text": expectedJSONStr,
							},
						},
						"role": "model",
					},
					"finishReason": "STOP",
				},
			},
		}
		b, _ := json.Marshal(resp)
		originalHTTPClient := geminiHTTPClient
		geminiHTTPClient = fakeHTTPClient(http.StatusOK, "text/event-stream", "data: "+string(b)+"\n\n")
		t.Cleanup(func() { geminiHTTPClient = originalHTTPClient })

		planner := NewLLMPlannerWithProvider(ProviderGemini, "test-key", "gemini-2.5-flash", "https://gemini.test")
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})
}

func TestOpenAIChatPlannerEmbedsJSONSchemaInSystemMessage(t *testing.T) {
	response := `data: {"choices":[{"delta":{"content":"{\"thought_summary\":\"done\",\"stop\":true,\"final_answer\":\"answer\",\"actions\":[{\"action\":\"none\",\"parameters\":{}}]}"}}]}` + "\n\n" + "data: [DONE]\n\n"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat map[string]any `json:"response_format"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if len(body.Messages) == 0 ||
			!strings.Contains(body.Messages[0].Content, "JSON Schema") ||
			!strings.Contains(body.Messages[0].Content, `"required":["thought_summary","stop","final_answer","actions"]`) {
			t.Errorf("planner system message is missing the JSON schema: %+v", body.Messages)
		}
		encodedFormat, _ := json.Marshal(body.ResponseFormat)
		if body.ResponseFormat["type"] != "json_schema" || strings.Contains(string(encodedFormat), "uniqueItems") {
			t.Errorf("direct OpenAI response_format is not compatible strict schema: %s", encodedFormat)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    req,
		}, nil
	})}

	_, _, err := (&openAIChatProvider{}).Plan(context.Background(), PlanRequest{
		Client:       client,
		Model:        "agent-planner",
		BaseURL:      "http://litellm.test/v1/chat/completions",
		SystemPrompt: "Choose the next action.",
		UserPrompt:   "Task goal: inspect the repository.",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLiteLLMPlannerUsesGatewayCompatibleJSONMode(t *testing.T) {
	response := `data: {"choices":[{"delta":{"content":"{\"thought_summary\":\"done\",\"stop\":true,\"final_answer\":\"answer\",\"actions\":[{\"action\":\"none\",\"parameters\":{}}]}"}}]}` + "\n\n" + "data: [DONE]\n\n"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat map[string]any `json:"response_format"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body.ResponseFormat["type"] != "json_object" {
			t.Errorf("LiteLLM response_format = %+v, want json_object", body.ResponseFormat)
		}
		if len(body.Messages) == 0 || !strings.Contains(body.Messages[0].Content, "JSON Schema") {
			t.Errorf("LiteLLM system message is missing schema instruction: %+v", body.Messages)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    req,
		}, nil
	})}

	_, _, err := (&liteLLMProvider{}).Plan(context.Background(), PlanRequest{
		Client:       client,
		Model:        "agent-planner",
		BaseURL:      "http://litellm.test/v1/chat/completions",
		SystemPrompt: "Choose the next action.",
		UserPrompt:   "Task goal: inspect the repository.",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLLMPlannerRoutesJITRetrievalBeforeProviderCall(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.RAG.ContextMode = "jit"
		cfg.RAG.SearchURL = "https://rag.test/mcp"
	}))
	providerCalls := 0
	planner := NewLLMPlannerWithProvider(ProviderOpenAIResponses, "test-key", "model", "https://llm.test/responses")
	planner.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, io.ErrUnexpectedEOF
	})}
	task := &types.Task{ID: "jit-pre-route", Goal: "数学科学术顾问有哪个人？", MaxSteps: 5, ToolBudget: 5, LLMCallBudget: 5}
	decision, err := planner.PlanNext(context.Background(), task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 0 || task.LLMCalls != 0 {
		t.Fatalf("provider_calls=%d llm_calls=%d, want zero before deterministic retrieval", providerCalls, task.LLMCalls)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Action != "rag_search" || decision.TokenUsage.TotalTokens != 0 {
		t.Fatalf("unexpected pre-routed decision: %+v", decision)
	}
}

type stubCompressor struct{ calls *int }

func (s stubCompressor) Compress(context.Context, *types.Task) (string, types.TokenUsage, error) {
	if s.calls != nil {
		*s.calls++
	}
	return "compressed evidence", types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, nil
}

func TestLLMPlannerIncludesCompressionUsage(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneContextCompressor: {}}
	}))
	task := &types.Task{ID: "compress", Goal: "goal", MaxSteps: 10, ToolBudget: 10, Trace: make([]types.StepTrace, 8)}
	body := `{"output":[{"content":[{"text":"{\"thought_summary\":\"done\",\"stop\":true,\"final_answer\":\"answer\",\"actions\":[{\"action\":\"none\",\"parameters\":{}}]}"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`
	p := NewLLMPlannerWithProvider(ProviderOpenAIResponses, "key", "model", "https://llm.test/responses")
	calls := 0
	p.Compressor = stubCompressor{calls: &calls}
	p.Client = fakeHTTPClient(http.StatusOK, "application/json", body)
	decision, err := p.PlanNext(context.Background(), task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.TokenUsage.TotalTokens != 17 {
		t.Fatalf("total usage = %+v, want 17", decision.TokenUsage)
	}
	if _, err := p.PlanNext(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("compressor calls = %d, want 1 after cached summary", calls)
	}
}

func TestUnmarshalDecisionFallback(t *testing.T) {
	rawInput := `thought_summary\nThe file content confirms that CAIE refers to the Cambridge Assessment International Education, specifically for AS level exams in 2026. This is sufficient to answer the prompt.{
  "thought_summary": "Finding files",
  "stop": false,
  "final_answer": "",
  "actions": [
    {
      "action": "find_files",
      "parameters": {
        "pattern": "*"
      }
    }
  ]
}`

	var decision PlanDecision
	err := unmarshalDecision(rawInput, &decision)
	if err != nil {
		t.Fatalf("failed to unmarshal with fallback: %v", err)
	}

	if len(decision.Actions) == 0 || decision.Actions[0].Action != "find_files" {
		t.Errorf("expected action find_files, got %+v", decision.Actions)
	}
	if decision.ThoughtSummary != "Finding files" {
		t.Errorf("expected thought_summary 'Finding files', got %q", decision.ThoughtSummary)
	}
}

func TestUnmarshalDecisionRejectsSchemaEcho(t *testing.T) {
	rawInput := `{"type":"object","additionalProperties":false,"properties":{"actions":{"type":"array"}}}`

	var decision PlanDecision
	err := unmarshalDecision(rawInput, &decision)
	if err == nil {
		t.Fatalf("schema echo was accepted as a decision: %+v", decision)
	}
	if !strings.Contains(err.Error(), `missing required field "thought_summary"`) {
		t.Fatalf("error = %q, want missing required field", err)
	}
}

func TestUnmarshalDecisionSkipsSchemaEchoBeforeDecision(t *testing.T) {
	rawInput := `{"type":"object","properties":{"actions":{"type":"array"}}}
The schema above describes the output.
{"thought_summary":"done","stop":true,"final_answer":"answer","actions":[{"action":"none","parameters":{}}]}`

	var decision PlanDecision
	if err := unmarshalDecision(rawInput, &decision); err != nil {
		t.Fatalf("failed to find decision after schema echo: %v", err)
	}
	if !decision.Stop || decision.FinalAnswer != "answer" || len(decision.Actions) != 1 || decision.Actions[0].Action != "none" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

// TestLLMPlannerTokenThresholdTrigger is the regression test for the
// context_compression_token_threshold feature: compression must fire when the
// accumulated TotalTokens across new traces exceeds the configured token
// threshold, even when the step-count threshold has NOT been reached.
func TestLLMPlannerTokenThresholdTrigger(t *testing.T) {
	// Enable the context compressor scene.
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneContextCompressor: {}}
		// Set a very high trace threshold so only the token threshold fires.
		cfg.LLM.ContextCompressionTraceThreshold = 999
		// Set token threshold to 100 tokens total.
		cfg.LLM.ContextCompressionTokenThreshold = 100
	}))

	// Build a task with only 3 trace steps, but each carrying 40 tokens = 120 total > 100 threshold.
	traces := []types.StepTrace{
		{Step: 0, Action: "find_files", TokenUsage: types.TokenUsage{TotalTokens: 40}},
		{Step: 1, Action: "read_file", TokenUsage: types.TokenUsage{TotalTokens: 40}},
		{Step: 2, Action: "web_search", TokenUsage: types.TokenUsage{TotalTokens: 40}},
	}
	task := &types.Task{ID: "tok-thr", Goal: "goal", MaxSteps: 10, ToolBudget: 10, Trace: traces}

	body := `{"output":[{"content":[{"text":"{\"thought_summary\":\"done\",\"stop\":true,\"final_answer\":\"answer\",\"actions\":[{\"action\":\"none\",\"parameters\":{}}]}"}]}],"usage":{"input_tokens":5,"output_tokens\":2,"total_tokens":7}}`
	p := NewLLMPlannerWithProvider(ProviderOpenAIResponses, "key", "model", "https://llm.test/responses")
	calls := 0
	p.Compressor = stubCompressor{calls: &calls}
	p.Client = fakeHTTPClient(http.StatusOK, "application/json", body)

	_, _ = p.PlanNext(context.Background(), task, nil)

	// Compressor must have been called exactly once despite trace count (3) < trace threshold (999).
	if calls != 1 {
		t.Fatalf("compressor calls = %d, want 1 (token threshold should have triggered compression)", calls)
	}
}
