package improvement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/harness"
)

// EvidenceRef is a bounded pointer to the evidence supporting a lesson.
// The evidence itself (probe traces, compose reports) lives wherever the
// refinement run stored it; the lesson keeps the summary needed to judge
// reuse without re-reading everything.
type EvidenceRef struct {
	Kind    string `json:"kind"` // e.g. "probe-contrast"
	Summary string `json:"summary"`
	URI     string `json:"uri,omitempty"`
}

// Lesson is one adopted harness improvement with its supporting evidence.
// It is the improvement-harness counterpart of brain.Claim: instead of a
// fact about the world, it records a fact about what improved a harness.
type Lesson struct {
	ID            string         `json:"id"` // sha256 of canonical content; assigned by Store.Record
	Team          string         `json:"team"`
	HarnessBefore string         `json:"harness_before"` // base harness snapshot digest
	HarnessAfter  string         `json:"harness_after"`  // refined harness snapshot digest
	Edits         []harness.Edit `json:"edits"`          // the adopted edits
	Evidence      []EvidenceRef  `json:"evidence"`
	ContrastDelta string         `json:"contrast_delta"` // human-readable improvement, e.g. "pass 0.50 -> 1.00"
	Confidence    float64        `json:"confidence"`     // [0,1]
	ObservedAt    time.Time      `json:"observed_at"`
	Source        string         `json:"source"` // e.g. "harness-refine:compose"
}

// Retraction is a logical (non-destructive) withdrawal of a lesson.
type Retraction struct {
	LessonID    string    `json:"lesson_id"`
	Reason      string    `json:"reason"`
	RetractedAt time.Time `json:"retracted_at"`
}

// lessonID computes the content address of a lesson. Identity covers team,
// the harness transition, the edits, and the contrast delta: re-recording
// the same improvement is idempotent.
func lessonID(l Lesson) (string, error) {
	payload := struct {
		Team     string         `json:"team"`
		Before   string         `json:"harness_before"`
		After    string         `json:"harness_after"`
		Edits    []harness.Edit `json:"edits"`
		Delta    string         `json:"contrast_delta"`
		Evidence []EvidenceRef  `json:"evidence"`
	}{
		Team: l.Team, Before: l.HarnessBefore, After: l.HarnessAfter,
		Edits: l.Edits, Delta: l.ContrastDelta, Evidence: l.Evidence,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize lesson: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (l Lesson) validate() error {
	if strings.TrimSpace(l.Team) == "" {
		return fmt.Errorf("team is required")
	}
	if strings.TrimSpace(l.HarnessBefore) == "" || strings.TrimSpace(l.HarnessAfter) == "" {
		return fmt.Errorf("harness_before and harness_after are required")
	}
	if l.HarnessBefore == l.HarnessAfter {
		return fmt.Errorf("harness_before and harness_after must differ")
	}
	if len(l.Edits) == 0 {
		return fmt.Errorf("at least one edit is required")
	}
	if l.Confidence < 0 || l.Confidence > 1 {
		return fmt.Errorf("confidence must be in [0,1], got %v", l.Confidence)
	}
	if err := harness.Validate(harness.EditSet{Edits: l.Edits}); err != nil {
		return fmt.Errorf("lesson edits invalid: %w", err)
	}
	return nil
}
