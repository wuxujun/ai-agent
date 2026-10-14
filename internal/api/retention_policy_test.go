package api

import (
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
	"strings"
	"testing"
	"time"
)

func TestRetentionAssessmentNeverInfersAgeOrEligibility(t *testing.T) {
	yes, no := true, false
	count := 0
	for _, status := range []types.TaskStatus{types.StatusCreated, types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused, types.StatusPartial, types.StatusCompleted, types.StatusFailed, "invalid"} {
		row := store.RetentionObservation{ID: "task", Status: status, TraceCount: &count, LeaseActive: &no, ApprovalsUnresolved: &no}
		got := assessRetention(row, 30, nil)
		want := "blocked"
		if status == types.StatusCompleted || status == types.StatusFailed {
			want = "unknown"
		}
		if got.Assessment != want || got.TerminalAgeSource != "unavailable" || got.ContentHash != nil || got.EstimatedTaskBytes != nil {
			t.Fatal("timestamps/metadata treated as execution authorization")
		}
		row.LeaseActive = &yes
		if got := assessRetention(row, 30, nil); got.Assessment != "blocked" || !strings.Contains(strings.Join(got.Reasons, ","), "active_execution_lease") {
			t.Fatal("active lease ignored")
		}
		row.LeaseActive = &no
		row.ApprovalsUnresolved = &yes
		if got := assessRetention(row, 0, nil); got.Assessment != "blocked" || !strings.Contains(strings.Join(got.Reasons, ","), "unresolved_approvals") {
			t.Fatal("unclosed approval ignored")
		}
	}
}

func TestRetentionTrustedLifecycleRemainsDiagnostic(t *testing.T) {
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	for _, tc := range []struct {
		name                       string
		version                    int64
		since, maint               *time.Time
		assessment, source, reason string
	}{
		{"recent maintenance", 3, &old, &recent, "blocked", "store_lifecycle", "retention_window_not_elapsed"},
		{"aged", 3, &old, &old, "unknown", "store_lifecycle", "joint_snapshot_unverified"},
		{"legacy", 0, &old, &old, "unknown", "unavailable", "unknown_terminal_age"},
		{"missing terminal", 3, nil, &old, "unknown", "unavailable", "unknown_terminal_age"},
		{"future clock", 3, &old, &future, "unknown", "unavailable", "unknown_terminal_age"},
		{"invalid order", 3, &recent, &old, "unknown", "unavailable", "unknown_terminal_age"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := store.RetentionObservation{ID: "task", Status: types.StatusFailed, TerminalSince: tc.since, LastMaintenanceAt: tc.maint, MaintenanceVersion: tc.version}
			got := assessRetention(row, 30, nil)
			if got.Assessment != tc.assessment || got.TerminalAgeSource != tc.source || !strings.Contains(strings.Join(got.Reasons, ","), tc.reason) {
				t.Fatalf("assessment: %+v", got)
			}
		})
	}
}
