package types

import "time"

type AnswerAuditFinding struct {
	Kind     string `json:"kind"`
	Detail   string `json:"detail,omitempty"`
	SourceID string `json:"source_id,omitempty"`
}

type AnswerAuditStage struct {
	Name        string               `json:"name"`
	Status      string               `json:"status"`
	Reason      string               `json:"reason,omitempty"`
	Fingerprint string               `json:"fingerprint,omitempty"`
	Findings    []AnswerAuditFinding `json:"findings,omitempty"`
	TokenUsage  TokenUsage           `json:"token_usage,omitempty"`
	DurationMS  int64                `json:"duration_ms,omitempty"`
}

type AnswerAuditReport struct {
	PipelineVersion string             `json:"pipeline_version"`
	DraftHash       string             `json:"draft_hash"`
	EvidenceHash    string             `json:"evidence_hash"`
	StartedAt       time.Time          `json:"started_at"`
	CompletedAt     time.Time          `json:"completed_at"`
	FinalConfidence string             `json:"final_confidence,omitempty"`
	Enforcement     string             `json:"enforcement"`
	Publishable     bool               `json:"publishable"`
	Stages          []AnswerAuditStage `json:"stages"`
}
