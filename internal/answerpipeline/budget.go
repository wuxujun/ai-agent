package answerpipeline

import (
	"sync"

	"github.com/wuxujun/ai-agent/internal/types"
)

type auditBudget struct {
	mu        sync.Mutex
	capacity  int
	committed int
	reserved  int
	unlimited bool
}

type tokenLease struct {
	budget *auditBudget
	grant  int
	once   sync.Once
}

func newAuditBudget(task *types.Task, reserve int) *auditBudget {
	if task == nil || task.TokenBudget <= 0 || reserve <= 0 {
		return &auditBudget{unlimited: true}
	}
	used := taskTokenUsage(task)
	remaining := task.TokenBudget - used
	if remaining < 0 {
		remaining = 0
	}
	if remaining > reserve {
		remaining = reserve
	}
	return &auditBudget{capacity: remaining}
}

func (b *auditBudget) reserveTokens(request int) (*tokenLease, bool) {
	return b.reserveTokensKeeping(request, 0)
}

func (b *auditBudget) reserveTokensKeeping(request, protected int) (*tokenLease, bool) {
	if b == nil || request <= 0 || b.unlimited {
		return &tokenLease{}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if request+protected > b.capacity-b.committed-b.reserved {
		return nil, false
	}
	b.reserved += request
	return &tokenLease{budget: b, grant: request}, true
}

func (l *tokenLease) commit(actual int) {
	if l == nil || l.budget == nil {
		return
	}
	l.once.Do(func() {
		if actual < 0 {
			actual = 0
		}
		l.budget.mu.Lock()
		l.budget.reserved -= l.grant
		l.budget.committed += actual
		l.budget.mu.Unlock()
	})
}

func taskTokenUsage(task *types.Task) int {
	if task == nil {
		return 0
	}
	total := 0
	for _, trace := range task.Trace {
		total += trace.TokenUsage.TotalTokens
	}
	return total
}
