package types

import "time"

type SessionStatus string

const (
	SessionStatusActive   SessionStatus = "active"
	SessionStatusArchived SessionStatus = "archived"
)

// Session groups multiple tasks under one tenant-scoped conversation.
type Session struct {
	ID           string        `json:"id"`
	TenantID     string        `json:"tenant_id"`
	Title        string        `json:"title"`
	Status       SessionStatus `json:"status"`
	NextSequence int64         `json:"-"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}
