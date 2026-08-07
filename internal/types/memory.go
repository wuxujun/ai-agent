package types

import "time"

// Memory represents a long-term memory stored in the system.
type Memory struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	SessionID   string    `json:"session_id,omitempty"`
	TaskID      string    `json:"task_id"`
	Goal        string    `json:"goal"`
	FinalAnswer string    `json:"final_answer"`
	KeyFindings string    `json:"key_findings"`
	Timestamp   time.Time `json:"timestamp"`
	Embedding   []float32 `json:"embedding,omitempty"`
}
