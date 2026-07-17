package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Reaudit explicitly runs the answer pipeline for an already-terminal task.
// It intentionally bypasses Engine.Next's terminal-task short circuit while
// retaining tenant, call, cost, and token budget enforcement.
func (e *Engine) Reaudit(ctx context.Context, task *types.Task, force bool) (*types.AnswerAuditReport, error) {
	if e == nil || e.AnswerPipeline == nil {
		return nil, fmt.Errorf("answer pipeline is unavailable")
	}
	if task == nil || !types.IsTerminalTaskStatus(task.Status) {
		return nil, fmt.Errorf("only terminal tasks can be re-audited")
	}
	if strings.TrimSpace(task.FinalAnswer) == "" {
		return nil, fmt.Errorf("task has no answer to audit")
	}
	ctx = store.WithTenantScope(ctx, task.TenantID)
	if e.Store != nil {
		if ledger, ok := e.Store.(types.TenantUsageLedger); ok {
			ctx = llmcore.WithTenantUsageLedger(ctx, ledger)
		}
	}
	ctx = llmcore.WithTaskBudget(ctx, task)
	cfg := config.Get().AnswerPipeline
	if cfg.Enabled {
		ctx = llmcore.WithAnswerAuditReserve(ctx, cfg.AuditTokenReserve)
	}
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	previous := types.CloneTask(task)
	if force {
		task.AnswerAudit = nil
	}
	report, err := e.AnswerPipeline.Process(ctx, task, string(e.Mode))
	if err != nil {
		*task = *previous
		return nil, err
	}
	if report == nil {
		*task = *previous
		return nil, fmt.Errorf("answer pipeline is disabled")
	}
	return report, nil
}
