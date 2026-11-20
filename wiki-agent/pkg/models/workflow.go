package models

import "time"

// Task is a unit of software-development work submitted to the Orchestrator.
type Task struct {
	ID          string `json:"id"`
	Goal        string `json:"goal"`
	Description string `json:"description,omitempty"`
}

// Step is a single executable action within a Plan. Every step must cite the
// verified knowledge (chunk IDs) it is based on.
type Step struct {
	ID         string   `json:"id"`
	Action     string   `json:"action"`
	Rationale  string   `json:"rationale"`
	References []string `json:"references"`
}

// Plan is the Planner's output for a Task.
type Plan struct {
	TaskID    string           `json:"task_id"`
	Iteration int              `json:"iteration"`
	Steps     []Step           `json:"steps"`
	Knowledge []RetrievedChunk `json:"knowledge"`
	CreatedAt time.Time        `json:"created_at"`
}

// Finding is what an Executor reports back after running a Step. Findings are
// submitted to the Wiki Agent and must pass the Validation Gateway.
type Finding struct {
	StepID     string    `json:"step_id"`
	Source     string    `json:"source"`
	URI        string    `json:"uri"`
	Summary    string    `json:"summary"`
	Content    string    `json:"content"`
	Confidence float64   `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
}

// State is a node in the Orchestrator state machine.
type State string

const (
	StatePending   State = "pending"
	StatePlanning  State = "planning"
	StateExecuting State = "executing"
	StateVerifying State = "verifying"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// Agent names used in inter-agent messages.
const (
	AgentOrchestrator = "orchestrator"
	AgentPlanner      = "planner"
	AgentExecutor     = "executor"
	AgentWiki         = "wiki"
)

// Message is an inter-agent message routed by the Orchestrator.
type Message struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

// Transition records a state change.
type Transition struct {
	From   State     `json:"from"`
	To     State     `json:"to"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Workflow is the full, observable record of a Task's lifecycle.
type Workflow struct {
	Task        Task               `json:"task"`
	State       State              `json:"state"`
	Iteration   int                `json:"iteration"`
	Plan        *Plan              `json:"plan,omitempty"`
	Findings    []Finding          `json:"findings,omitempty"`
	Validations []ValidationResult `json:"validations,omitempty"`
	History     []Transition       `json:"history"`
	Messages    []Message          `json:"messages"`
	Error       string             `json:"error,omitempty"`
}

// PlanRequest asks the Planner for a plan. Feedback carries verification
// violations from the previous iteration so the Planner can re-plan.
type PlanRequest struct {
	Task      Task        `json:"task"`
	Iteration int         `json:"iteration"`
	Feedback  []Violation `json:"feedback,omitempty"`
}
