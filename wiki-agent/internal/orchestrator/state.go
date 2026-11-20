package orchestrator

import (
	"errors"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// ErrInvalidTransition is returned for transitions not in the state table.
var ErrInvalidTransition = errors.New("orchestrator: invalid state transition")

var transitions = map[models.State][]models.State{
	models.StatePending:   {models.StatePlanning, models.StateCancelled},
	models.StatePlanning:  {models.StateExecuting, models.StateFailed, models.StateCancelled},
	models.StateExecuting: {models.StateVerifying, models.StateFailed, models.StateCancelled},
	models.StateVerifying: {models.StateCompleted, models.StatePlanning, models.StateFailed, models.StateCancelled},
}

// CanTransition reports whether from -> to is permitted.
func CanTransition(from, to models.State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// machine applies validated transitions to a workflow record.
type machine struct {
	wf  *models.Workflow
	now func() time.Time
}

func newMachine(task models.Task, now func() time.Time) *machine {
	return &machine{
		wf:  &models.Workflow{Task: task, State: models.StatePending, History: []models.Transition{}, Messages: []models.Message{}},
		now: now,
	}
}

func (m *machine) transition(to models.State, reason string) error {
	from := m.wf.State
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	m.wf.State = to
	m.wf.History = append(m.wf.History, models.Transition{From: from, To: to, Reason: reason, At: m.now().UTC()})
	return nil
}

func (m *machine) send(from, to, kind, summary string) {
	m.wf.Messages = append(m.wf.Messages, models.Message{From: from, To: to, Kind: kind, Summary: summary, At: m.now().UTC()})
}
