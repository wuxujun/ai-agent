package planner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
