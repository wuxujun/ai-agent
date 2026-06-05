package types

// AgentRole identifies which agent produced a StepTrace entry in multi-agent mode.
type AgentRole string

const (
	AgentRolePlanner    AgentRole = "planner"
	AgentRoleResearcher AgentRole = "researcher"
	AgentRoleWriter     AgentRole = "writer"
	AgentRoleSingle     AgentRole = "" // default single-agent mode
)

type Evidence struct {
	Path  string   `json:"path"`
	Lines []string `json:"lines"`
	Query string   `json:"query"`
}

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type StepTrace struct {
	Step        int        `json:"step"`
	Goal        string     `json:"goal"`
	Action      string     `json:"action"`
	Query       string     `json:"query"`
	Observation string     `json:"observation"`
	Evidence    []Evidence `json:"evidence"`
	// Error holds the failure reason when this action failed. It is empty for
	// successful actions. A non-empty Error means the step was recorded as an
	// observation for the planner to react to, rather than aborting the task.
	Error string `json:"error,omitempty"`
	// AgentRole identifies the agent that produced this trace entry.
	// Empty for single-agent (legacy/eino/adk) modes; set in multi-agent mode.
	AgentRole  AgentRole  `json:"agent_role,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
}

type TaskStatus string

const (
	StatusCreated          TaskStatus = "created"
	StatusRunning          TaskStatus = "running"
	StatusAwaitingApproval TaskStatus = "awaiting_approval"
	StatusCompleted        TaskStatus = "completed"
	StatusFailed           TaskStatus = "failed"
)

type RiskLevel string

const (
	RiskLevelLow  RiskLevel = "low"
	RiskLevelHigh RiskLevel = "high"
)

type ApprovalRequest struct {
	TaskID           string         `json:"task_id"`
	Action           string         `json:"action"`
	RiskLevel        RiskLevel      `json:"risk_level"`
	Workspace        string         `json:"workspace"`
	Parameters       map[string]any `json:"parameters,omitempty"`
	ParameterSummary []string       `json:"parameter_summary,omitempty"`
	Preview          string         `json:"preview,omitempty"`
}

type Task struct {
	ID          string      `json:"id"`
	Goal        string      `json:"goal"`
	Status      TaskStatus  `json:"status"`
	MaxSteps    int         `json:"max_steps"`
	StepCount   int         `json:"step_count"`
	Workspace   string      `json:"workspace"`
	Hypothesis  string      `json:"hypothesis"`
	Unresolved  []string    `json:"unresolved"`
	ToolBudget  int         `json:"tool_budget"`
	TokenBudget int         `json:"token_budget"`
	Trace       []StepTrace `json:"trace"`
	FinalAnswer string      `json:"final_answer"`
	Memories    []Memory    `json:"memories,omitempty"`
}
