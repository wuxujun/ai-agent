package orchestrator

import (
	"context"
	"iter"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type mockLLM struct {
	response *model.LLMResponse
	calls    int
}

func (m *mockLLM) Name() string {
	return "mock-llm"
}

func (m *mockLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(m.response, nil)
	}
}

func TestAdkNextExecutesModelAndCompletes(t *testing.T) {
	mockResponse := &model.LLMResponse{
		Content: &genai.Content{
			Role: string(genai.RoleModel),
			Parts: []*genai.Part{
				{
					Text: "The answer is secret-123",
				},
			},
		},
		TurnComplete: true,
	}

	m := &mockLLM{response: mockResponse}
	engine := &Engine{
		Mode:     ModeAdk,
		AdkModel: m,
	}

	task := &types.Task{
		ID:         "task-adk-1",
		Goal:       "find the key secret",
		Status:     "created",
		MaxSteps:   5,
		ToolBudget: 5,
		Workspace:  "./workspace",
	}

	err := engine.Next(context.Background(), task)
	if err != nil {
		t.Fatalf("ADK Next returned error: %v", err)
	}

	if m.calls != 1 {
		t.Fatalf("expected 1 model call, got %d", m.calls)
	}

	if task.Status != types.StatusCompleted {
		t.Fatalf("expected task status completed, got %s", task.Status)
	}

	if task.FinalAnswer != "The answer is secret-123" {
		t.Fatalf("expected final answer 'The answer is secret-123', got '%s'", task.FinalAnswer)
	}
}
