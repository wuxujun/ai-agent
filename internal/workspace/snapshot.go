package workspace

import "context"

type RunRequest struct {
	SessionID string `json:"session_id"`
	UserInput string `json:"user_input"`
}

type RunResult struct {
	Answer string `json:"answer"`
	State  *State `json:"state,omitempty"`
}

type State struct {
	SessionID    string           `json:"session_id"`
	UserInput    string           `json:"user_input"`
	Goal         string           `json:"goal"`
	NeedTool     bool             `json:"need_tool"`
	ToolName     string           `json:"tool_name"`
	ToolInput    map[string]any   `json:"tool_input"`
	Observations []string         `json:"observations"`
	FinalAnswer  string           `json:"final_answer"`
	ToolCalls    []ToolCallRecord `json:"tool_calls"`
	Status       string           `json:"status"`
	Error        string           `json:"error,omitempty"`
}

type ToolCallRecord struct {
	ToolName string         `json:"tool_name"`
	Input    map[string]any `json:"input"`
	Output   map[string]any `json:"output,omitempty"`
	Success  bool           `json:"success"`
	Error    string         `json:"error,omitempty"`
}

type Planner interface {
	Plan(ctx context.Context, state *State) error
}

type Responder interface {
	Build(ctx context.Context, state *State) error
}

type Tool interface {
	Name() string
	Description() string
	Call(ctx context.Context, input map[string]any) (*ToolResult, error)
}

type ToolResult struct {
	Data map[string]any `json:"data"`
}
