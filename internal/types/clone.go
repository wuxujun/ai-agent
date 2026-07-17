package types

// CloneTask returns a fully detached task snapshot suitable for stores,
// asynchronous indexing, and read-only audit adapters.
func CloneTask(task *Task) *Task {
	if task == nil {
		return nil
	}
	cloned := *task
	cloned.Unresolved = append([]string(nil), task.Unresolved...)
	cloned.Trace = make([]StepTrace, len(task.Trace))
	for i, trace := range task.Trace {
		cloned.Trace[i] = trace
		cloned.Trace[i].Evidence = make([]Evidence, len(trace.Evidence))
		for j, evidence := range trace.Evidence {
			cloned.Trace[i].Evidence[j] = evidence
			cloned.Trace[i].Evidence[j].Lines = append([]string(nil), evidence.Lines...)
		}
	}
	cloned.Memories = make([]Memory, len(task.Memories))
	for i, memory := range task.Memories {
		cloned.Memories[i] = memory
		cloned.Memories[i].Embedding = append([]float32(nil), memory.Embedding...)
	}
	if task.AnswerAudit != nil {
		report := *task.AnswerAudit
		report.Stages = make([]AnswerAuditStage, len(task.AnswerAudit.Stages))
		for i, stage := range task.AnswerAudit.Stages {
			report.Stages[i] = stage
			report.Stages[i].Findings = append([]AnswerAuditFinding(nil), stage.Findings...)
		}
		cloned.AnswerAudit = &report
	}
	return &cloned
}
