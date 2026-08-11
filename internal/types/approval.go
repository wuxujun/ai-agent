package types

import (
	"errors"
	"strings"
	"time"
)

type DurableApprovalStatus string

const (
	ApprovalPending  DurableApprovalStatus = "pending"
	ApprovalApproved DurableApprovalStatus = "approved"
	ApprovalRejected DurableApprovalStatus = "rejected"
	ApprovalExpired  DurableApprovalStatus = "expired"
	ApprovalConsumed DurableApprovalStatus = "consumed"
)

// DurableApproval is the persistence representation of one approval gate.
// Request must contain only the existing redacted API-safe preview. Sensitive
// action and resolution parameters belong in the encrypted payload fields and
// must never be serialized through the ordinary Task API.
type DurableApproval struct {
	ID                string
	TaskID            string
	TenantID          string
	Request           ApprovalRequest
	ActionPayload     []byte
	ResolutionPayload []byte
	Status            DurableApprovalStatus
	Version           int64
	Owner             string
	LeaseExpiresAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ResolvedAt        time.Time
}

func (a *DurableApproval) Validate() error {
	if a == nil {
		return errors.New("approval must not be nil")
	}
	if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.TaskID) == "" || strings.TrimSpace(a.TenantID) == "" {
		return errors.New("approval id, task id, and tenant id are required")
	}
	if a.Request.ID != a.ID || a.Request.TaskID != a.TaskID {
		return errors.New("approval request identity does not match durable record")
	}
	if strings.TrimSpace(a.Request.Action) == "" || a.Request.RiskLevel != RiskLevelHigh {
		return errors.New("durable approval requires a high-risk action")
	}
	if len(a.ActionPayload) == 0 {
		return errors.New("encrypted action payload is required")
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	switch a.Status {
	case ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalExpired, ApprovalConsumed:
	default:
		return errors.New("invalid durable approval status")
	}
	if a.Version < 0 {
		return errors.New("approval version must be non-negative")
	}
	return nil
}

func CanTransitionApproval(from, to DurableApprovalStatus) bool {
	switch from {
	case ApprovalPending:
		return to == ApprovalApproved || to == ApprovalRejected || to == ApprovalExpired
	case ApprovalApproved:
		return to == ApprovalConsumed || to == ApprovalExpired
	case ApprovalRejected:
		return to == ApprovalConsumed
	default:
		return false
	}
}

// CloneDurableApproval returns a deep copy safe for crossing store boundaries.
func CloneDurableApproval(a *DurableApproval) *DurableApproval {
	if a == nil {
		return nil
	}
	cloned := *a
	cloned.Request.Parameters = cloneAnyMap(a.Request.Parameters)
	cloned.Request.ParameterSummary = append([]string(nil), a.Request.ParameterSummary...)
	cloned.ActionPayload = append([]byte(nil), a.ActionPayload...)
	cloned.ResolutionPayload = append([]byte(nil), a.ResolutionPayload...)
	return &cloned
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		switch typed := value.(type) {
		case map[string]any:
			cloned[key] = cloneAnyMap(typed)
		case []any:
			items := make([]any, len(typed))
			copy(items, typed)
			cloned[key] = items
		default:
			cloned[key] = value
		}
	}
	return cloned
}
