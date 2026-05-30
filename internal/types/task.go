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

type StepTrace struct {
	Step        int        `json:"step"`
	Goal        string     `json:"goal"`
	Action      string     `json:"action"`
	Query       string     `json:"query"`
	Observation string     `json:"observation"`
	Evidence    []Evidence `json:"evidence"`
	// AgentRole identifies the agent that produced this trace entry.
	// Empty for single-agent (legacy/eino/adk) modes; set in multi-agent mode.
	AgentRole AgentRole `json:"agent_role,omitempty"`
}

type TaskStatus string

const (
	StatusCreated   TaskStatus = "created"
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
)

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
	Trace       []StepTrace `json:"trace"`
	FinalAnswer string      `json:"final_answer"`
	Memories    []Memory    `json:"memories,omitempty"`
}

