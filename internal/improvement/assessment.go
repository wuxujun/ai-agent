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

// ProposedCandidate is one editor candidate recorded with an assessment.
type ProposedCandidate struct {
	Index int            `json:"index"`
	Kind  string         `json:"kind"` // "general" | "domain"
	Edits []harness.Edit `json:"edits"`
}

// Assessment is a harnessing-level observation from live traffic: a completed
// task's outcome paired with the edits an editor would have proposed for it.
// Unlike a Lesson it carries no probe evidence and never drives automatic
// adoption; operators promote interesting assessments to offline probe runs
// (harness-refine run) which may produce Lessons.
type Assessment struct {
	ID            string              `json:"id"` // sha256(task + harness + candidates)
	Team          string              `json:"team"`
	TaskID        string              `json:"task_id"`
	TaskStatus    string              `json:"task_status"`
	StepCount     int                 `json:"step_count"`
	ErrorSteps    int                 `json:"error_steps"`
	HarnessDigest string              `json:"harness_digest"`
	Candidates    []ProposedCandidate `json:"candidates"`
	ObservedAt    time.Time           `json:"observed_at"`
	Source        string              `json:"source"` // e.g. "refine:assess"
}

func assessmentID(a Assessment) (string, error) {
	payload := struct {
		TaskID  string              `json:"task_id"`
		Harness string              `json:"harness_digest"`
		Cands   []ProposedCandidate `json:"candidates"`
	}{
		TaskID: a.TaskID, Harness: a.HarnessDigest, Cands: a.Candidates,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize assessment: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (a Assessment) validate() error {
	if strings.TrimSpace(a.Team) == "" {
		return fmt.Errorf("team is required")
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return fmt.Errorf("task_id is required")
	}
	if strings.TrimSpace(a.HarnessDigest) == "" {
		return fmt.Errorf("harness_digest is required")
	}
	return nil
}
