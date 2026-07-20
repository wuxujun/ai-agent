package multiagent

import (
	"encoding/json"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

const (
	VerifierDraftCheckpointTraceAction = "verify_draft"
	verifierRetryReason                = "final_verification_retry_required"
)

type verifierDraftCheckpoint struct {
	Version           int               `json:"version"`
	Draft             VerificationDraft `json:"draft"`
	Evidence          []StepEvidence    `json:"evidence"`
	ExecutionComplete bool              `json:"execution_complete"`
	ExecutionReason   string            `json:"execution_reason,omitempty"`
}

func appendVerifierDraftCheckpoint(task *types.Task, draft *VerificationDraft, evidence []StepEvidence, executionComplete bool, executionReason string) verifierDraftCheckpoint {
	checkpoint := verifierDraftCheckpoint{
		Version:           1,
		Draft:             *draft,
		Evidence:          append([]StepEvidence(nil), evidence...),
		ExecutionComplete: executionComplete,
		ExecutionReason:   executionReason,
	}
	encoded, _ := json.Marshal(checkpoint)
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      VerifierDraftCheckpointTraceAction,
		Query:       "verifier_draft_checkpoint",
		Observation: string(encoded),
		AgentRole:   RoleVerifier,
		TokenUsage:  draft.TokenUsage,
	})
	task.StepCount++
	task.FinalAnswer = draft.FinalAnswer
	return checkpoint
}

func pendingVerifierDraft(task *types.Task) (verifierDraftCheckpoint, bool) {
	if task == nil {
		return verifierDraftCheckpoint{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action == "verify" && trace.Error == "" {
			return verifierDraftCheckpoint{}, false
		}
		if trace.Action != VerifierDraftCheckpointTraceAction {
			continue
		}
		var checkpoint verifierDraftCheckpoint
		if json.Unmarshal([]byte(trace.Observation), &checkpoint) != nil || checkpoint.Version != 1 || strings.TrimSpace(checkpoint.Draft.FinalAnswer) == "" {
			return verifierDraftCheckpoint{}, false
		}
		return checkpoint, true
	}
	return verifierDraftCheckpoint{}, false
}

// HasPendingVerifierDraft reports whether a partial multi-agent task can resume
// at Verify without rerunning Planner, Critic, Executor, or Draft.
func HasPendingVerifierDraft(task *types.Task) bool {
	_, ok := pendingVerifierDraft(task)
	return ok
}

func removeUnresolvedReason(task *types.Task, reason string) {
	if task == nil || len(task.Unresolved) == 0 {
		return
	}
	filtered := task.Unresolved[:0]
	for _, current := range task.Unresolved {
		if current != reason {
			filtered = append(filtered, current)
		}
	}
	task.Unresolved = filtered
}
