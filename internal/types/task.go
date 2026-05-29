package types

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
}
