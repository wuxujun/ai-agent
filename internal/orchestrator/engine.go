package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/model"
)

type Engine struct {
	Planner     planner.Planner
	Executor    executor.Executor
	Metrics     *metrics.Collector
	Mode        Mode
	AdkModel    model.LLM
	// Coordinator is required when Mode == ModeMultiAgent.
	Coordinator *multiagent.Coordinator
	// Store handles database persistence and long-term memory.
	Store       store.Store

	// einoRunner is compiled once and cached for the lifetime of the Engine.
	// A sync.Mutex guards lazy initialisation; unlike sync.Once, a failed
	// compilation attempt can be retried on the next call.
	einoMu     sync.Mutex
	einoRunner any  // compose.Runnable[*einoStepState, *types.Task] after successful compile
	einoReady  bool // true once einoRunner has been successfully compiled

	// adkRunner is compiled once and cached for reuse across all runAdkNext calls.
	adkOnce    sync.Once
	adkRunner  any // *runner.Runner
	adkErr     error
}

var tracer = otel.Tracer("ai-agent/orchestrator")

type Mode string

const (
	ModeEino       Mode = "eino"
	ModeLegacy     Mode = "legacy"
	ModeAdk        Mode = "adk"
	ModeStep       Mode = "step"
	ModeMultiAgent Mode = "multiagent"
)

func (e *Engine) Next(ctx context.Context, task *types.Task) (err error) {
	log.Printf("[Engine] Running next execution step for task %s (mode: %q)", task.ID, e.Mode)
	defer func() {
		if err != nil {
			log.Printf("[Engine Error] Task %s step execution failed: %v", task.ID, err)
			_ = SetTaskFailed(task, err.Error())
		}
	}()

	if task.StepCount == 0 && len(task.Memories) == 0 {
		var retrievedMems []types.Memory

		// 1. Try querying third-party RAG search URL if configured (query up to 5 candidates)
		if os.Getenv("AI_AGENT_RAG_SEARCH_URL") != "" {
			if extMems, extErr := memory.SearchThirdPartyRAG(ctx, task.Goal); extErr == nil && len(extMems) > 0 {
				retrievedMems = append(retrievedMems, extMems...)
				log.Printf("[Engine] Retrieved %d memories from third-party RAG URL for task %s", len(extMems), task.ID)
			} else if extErr != nil {
				log.Printf("[Engine Warning] Failed to query third-party RAG URL: %v", extErr)
			}
		}

		// 2. Query local Store for up to 5 candidates
		if e.Store != nil {
			log.Printf("[Engine] Querying local long-term memory for task: %s (Goal: %q)", task.ID, task.Goal)
			if emb, embErr := memory.GetEmbedding(ctx, task.Goal); embErr == nil {
				if mems, queryErr := e.Store.QueryMemories(ctx, task.Goal, emb, 5); queryErr == nil && len(mems) > 0 {
					for _, m := range mems {
						retrievedMems = append(retrievedMems, *m)
					}
					log.Printf("[Engine] Retrieved %d relevant local historical memories for task %s", len(mems), task.ID)
				}
			}
		}

		// 3. Deduplicate and limit to top 3 unique memories
		if len(retrievedMems) > 0 {
			deduped := memory.DeduplicateMemories(retrievedMems)
			if len(deduped) > 3 {
				deduped = deduped[:3]
			}
			task.Memories = deduped
			log.Printf("[Engine] Final RAG memories count after deduplication and filtering: %d for task %s", len(task.Memories), task.ID)
		}
	}


	switch e.Mode {
	case "", ModeEino:
		err = e.runEinoNext(ctx, task)
	case ModeLegacy:
		err = e.runLegacyNext(ctx, task)
	case ModeAdk:
		err = e.runAdkNext(ctx, task)
	case ModeStep:
		err = e.runStepNext(ctx, task)
	case ModeMultiAgent:
		err = e.runMultiAgentNext(ctx, task)
	default:
		err = fmt.Errorf("unsupported orchestrator mode: %s", e.Mode)
	}
	return err
}

func (e *Engine) runLegacyNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	log.Printf("[Engine] Running step %d/%d (budget: %d) for task %s", task.StepCount+1, task.MaxSteps, task.ToolBudget, task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "legacy"),
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Printf("[Engine] Task %s reached step limit (%d/%d) or budget limit (%d)", task.ID, task.StepCount, task.MaxSteps, task.ToolBudget)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "stopped by budget or max steps"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "budget_or_max_steps"))
		return nil
	}

	pStart := time.Now()
	decision, err := e.Planner.PlanNext(ctx, task)
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		log.Printf("[Engine Error] Planner failed for task %s: %v", task.ID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner failure")
		return err
	}

	log.Printf("[Engine] Task %s - Planner thought: %q | Action chosen: %q", task.ID, decision.ThoughtSummary, decision.Action)

	task.Hypothesis = decision.ThoughtSummary
	span.SetAttributes(
		attribute.String("agent.planner.action", decision.Action),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	if decision.Stop {
		log.Printf("[Engine] Task %s - Planner decided to stop. FinalAnswer: %q", task.ID, decision.FinalAnswer)
		_ = SetTaskCompleted(task, decision.FinalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "planner_stop"))
		return nil
	}

	log.Printf("[Engine] Task %s - Executing action %q with parameters: %+v", task.ID, decision.Action, decision.Parameters)
	xStart := time.Now()
	trace, err := e.Executor.Execute(ctx, task, decision)
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), err, decision.Action)
	}
	if err != nil {
		log.Printf("[Engine Error] Executor failed for task %s, action %q: %v", task.ID, decision.Action, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "executor failure")
		return err
	}

	log.Printf("[Engine] Task %s - Action execution success. Observation: %q", task.ID, trace.Observation)

	task.StepCount++
	task.ToolBudget--
	task.Trace = append(task.Trace, *trace)
	_ = SetTaskRunning(task)

	log.Printf("[Engine] Step %d completed for task %s. Remaining budget: %d", task.StepCount, task.ID, task.ToolBudget)

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	log.Printf("[Engine] Starting task %s to completion. Goal: %q", task.ID, task.Goal)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	if e.Metrics != nil {
		e.Metrics.IncRunAll()
	}

	for task.Status != types.StatusCompleted && task.Status != types.StatusFailed {
		select {
		case <-ctx.Done():
			log.Printf("[Engine Warning] Task %s canceled: %v", task.ID, ctx.Err())
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			return ctx.Err()
		default:
		}

		if err := e.Next(ctx, task); err != nil {
			log.Printf("[Engine Error] Execution step failed for task %s: %v", task.ID, err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "run_all failed")
			return err
		}
	}

	log.Printf("[Engine] Task %s finished. Status: %s | FinalAnswer: %q", task.ID, task.Status, task.FinalAnswer)
	span.SetAttributes(attribute.String("agent.task.final_answer", task.FinalAnswer))
	return nil
}

func stepFindTextFiles(task *types.Task) error {
	log.Printf("[Engine] Legacy static path - Finding text files for task %s", task.ID)
	task.Hypothesis = "Relevant evidence is likely inside text or markdown files"

	txtFiles, err := tools.FindFiles(task.Workspace, "*.txt")
	if err != nil {
		log.Printf("[Engine Error] Legacy static path - FindFiles (*.txt) failed: %v", err)
		return err
	}
	mdFiles, err := tools.FindFiles(task.Workspace, "*.md")
	if err != nil {
		log.Printf("[Engine Error] Legacy static path - FindFiles (*.md) failed: %v", err)
		return err
	}

	files := append(txtFiles, mdFiles...)
	if len(files) > 20 {
		files = files[:20]
	}

	log.Printf("[Engine] Legacy static path - Found %d text/markdown files for task %s", len(files), task.ID)

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "find_files",
		Query:       "*.txt, *.md",
		Observation: fmt.Sprintf("found %d candidate files", len(files)),
	})

	if len(files) == 0 {
		task.Unresolved = append(task.Unresolved, "no candidate text or markdown files found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepSearchKeyword(task *types.Task) error {
	query := lastWord(task.Goal)
	log.Printf("[Engine] Legacy static path - Searching keyword %q for task %s", query, task.ID)
	task.Hypothesis = "Search the most likely keyword in candidate text files"

	evidence, _, err := tools.SearchWithRG(task.Workspace, query, "*.txt")
	if err != nil {
		log.Printf("[Engine Error] Legacy static path - SearchWithRG failed: %v", err)
		return err
	}

	log.Printf("[Engine] Legacy static path - Found %d evidence items for task %s", len(evidence), task.ID)

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "search_text",
		Query:       query,
		Observation: fmt.Sprintf("found %d evidence items", len(evidence)),
		Evidence:    evidence,
	})

	if len(evidence) == 0 {
		task.Unresolved = append(task.Unresolved, "keyword not found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepReadBestFile(task *types.Task) error {
	log.Printf("[Engine] Legacy static path - Reading best file for task %s", task.ID)
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) == 0 {
		log.Printf("[Engine] Legacy static path - Not enough evidence to select a file for task %s", task.ID)
		_ = SetTaskCompleted(task, "not enough evidence to select a file")
		return nil
	}

	target := task.Trace[1].Evidence[0].Path
	log.Printf("[Engine] Legacy static path - Target best file identified: %s for task %s", target, task.ID)
	content, err := tools.ReadFile(task.Workspace, target)
	if err != nil {
		log.Printf("[Engine Error] Legacy static path - ReadFile failed: %v", err)
		return err
	}

	snippet := content
	if len(snippet) > 220 {
		snippet = snippet[:220]
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "read_file",
		Query:       target,
		Observation: "read file snippet: " + snippet,
	})

	_ = SetTaskCompleted(task, fmt.Sprintf("completed search; best candidate file: %s", target))
	return nil
}

func lastWord(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "TODO"
	}
	return parts[len(parts)-1]
}
