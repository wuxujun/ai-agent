package agent

import (
	"context"
	"fmt"
)

type ToolRegistry interface {
	Get(name string) (Tool, bool)
}

type Orchestrator struct {
	planner   Planner
	responder Responder
	tools     ToolRegistry
	maxSteps  int
}

func NewOrchestrator(planner Planner, responder Responder, tools ToolRegistry) *Orchestrator {
	return &Orchestrator{
		planner:   planner,
		responder: responder,
		tools:     tools,
		maxSteps:  3,
	}
}

func (o *Orchestrator) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	state := &State{
		SessionID:    req.SessionID,
		UserInput:    req.UserInput,
		Observations: []string{},
		ToolCalls:    []ToolCallRecord{},
		Status:       "running",
	}

	if err := o.planner.Plan(ctx, state); err != nil {
		return nil, err
	}

	for i := 0; i < o.maxSteps; i++ {
		if !state.NeedTool {
			break
		}

		tool, ok := o.tools.Get(state.ToolName)
		if !ok {
			return nil, fmt.Errorf("tool not found: %s", state.ToolName)
		}

		result, err := tool.Call(ctx, state.ToolInput)
		record := ToolCallRecord{
			ToolName: state.ToolName,
			Input:    state.ToolInput,
			Success:  err == nil,
		}
		if err != nil {
			record.Error = err.Error()
			state.ToolCalls = append(state.ToolCalls, record)
			state.Error = err.Error()
			break
		}

		record.Output = result.Data
		state.ToolCalls = append(state.ToolCalls, record)
		state.Observations = append(state.Observations, fmt.Sprintf("%s => %+v", state.ToolName, result.Data))

		state.NeedTool = false
	}

	if err := o.responder.Build(ctx, state); err != nil {
		return nil, err
	}

	state.Status = "completed"
	return &RunResult{
		Answer: state.FinalAnswer,
		State:  state,
	}, nil
}
