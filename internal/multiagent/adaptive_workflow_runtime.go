package multiagent

type adaptiveDAGEscalationError struct{ reason string }

func (e *adaptiveDAGEscalationError) Error() string {
	return e.reason
}
