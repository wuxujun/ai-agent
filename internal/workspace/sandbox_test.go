package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type plannerStub func(context.Context, *State) error

func (f plannerStub) Plan(ctx context.Context, state *State) error { return f(ctx, state) }

type responderStub func(context.Context, *State) error

func (f responderStub) Build(ctx context.Context, state *State) error { return f(ctx, state) }

type toolStub struct {
	result *ToolResult
	err    error
}

func (toolStub) Name() string        { return "lookup" }
func (toolStub) Description() string { return "test lookup" }
func (t toolStub) Call(context.Context, map[string]any) (*ToolResult, error) {
	return t.result, t.err
}

type registryStub map[string]Tool

func (r registryStub) Get(name string) (Tool, bool) {
	tool, ok := r[name]
	return tool, ok
}

func TestLegacyOrchestratorDocumentsExistingBehavior(t *testing.T) {
	planner := plannerStub(func(_ context.Context, state *State) error {
		state.NeedTool = true
		state.ToolName = "lookup"
		state.ToolInput = map[string]any{"query": "status"}
		return nil
	})
	responder := responderStub(func(_ context.Context, state *State) error {
		state.FinalAnswer = "done"
		return nil
	})
	runtime := NewOrchestrator(planner, responder, registryStub{
		"lookup": toolStub{result: &ToolResult{Data: map[string]any{"value": "ok"}}},
	})

	result, err := runtime.Run(t.Context(), RunRequest{SessionID: "session-1", UserInput: "check"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || result.State.Status != "completed" {
		t.Fatalf("Run() result = %+v", result)
	}
	if len(result.State.ToolCalls) != 1 || !result.State.ToolCalls[0].Success {
		t.Fatalf("tool calls = %+v", result.State.ToolCalls)
	}
	if len(result.State.Observations) != 1 || !strings.Contains(result.State.Observations[0], "lookup") {
		t.Fatalf("observations = %+v", result.State.Observations)
	}
}

func TestLegacyOrchestratorRejectsUnknownTool(t *testing.T) {
	responderCalled := false
	runtime := NewOrchestrator(
		plannerStub(func(_ context.Context, state *State) error {
			state.NeedTool = true
			state.ToolName = "missing"
			return nil
		}),
		responderStub(func(context.Context, *State) error {
			responderCalled = true
			return nil
		}),
		registryStub{},
	)

	_, err := runtime.Run(t.Context(), RunRequest{})
	if err == nil || !strings.Contains(err.Error(), "tool not found") {
		t.Fatalf("Run() error = %v", err)
	}
	if responderCalled {
		t.Fatal("responder was called after an unknown tool")
	}
}

func TestLegacyOrchestratorPropagatesPlannerAndResponderErrors(t *testing.T) {
	plannerErr := errors.New("planner failed")
	responderErr := errors.New("responder failed")
	tests := []struct {
		name      string
		planner   Planner
		responder Responder
		want      error
	}{
		{
			name: "planner",
			planner: plannerStub(func(context.Context, *State) error {
				return plannerErr
			}),
			responder: responderStub(func(context.Context, *State) error { return nil }),
			want:      plannerErr,
		},
		{
			name:      "responder",
			planner:   plannerStub(func(context.Context, *State) error { return nil }),
			responder: responderStub(func(context.Context, *State) error { return responderErr }),
			want:      responderErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOrchestrator(tt.planner, tt.responder, registryStub{}).Run(t.Context(), RunRequest{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
		})
	}
}
