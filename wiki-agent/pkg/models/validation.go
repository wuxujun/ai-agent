package models

import "time"

// ValidationStage identifies which side of the Wiki Agent a check guards.
type ValidationStage string

const (
	StageIngest ValidationStage = "ingest"
	StageEmit   ValidationStage = "emit"
)

// ValidationStatus is the gateway verdict.
type ValidationStatus string

const (
	StatusAccepted ValidationStatus = "accepted"
	StatusRejected ValidationStatus = "rejected"
)

// Violation describes a single failed rule.
type Violation struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// ValidationResult is produced by the Validation Gateway for every item it inspects.
type ValidationResult struct {
	Status     ValidationStatus `json:"status"`
	Stage      ValidationStage  `json:"stage"`
	Confidence float64          `json:"confidence"`
	Violations []Violation      `json:"violations,omitempty"`
	RuleSet    string           `json:"rule_set"`
	CheckedAt  time.Time        `json:"checked_at"`
	ExpiresAt  *time.Time       `json:"expires_at,omitempty"`
}

// Accepted reports whether the item passed every rule.
func (r ValidationResult) Accepted() bool { return r.Status == StatusAccepted }
