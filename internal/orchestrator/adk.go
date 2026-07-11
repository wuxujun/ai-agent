package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type contextKey string

const (
	workspaceKey contextKey = "workspace"
	taskKey      contextKey = "task"
)

// FindFilesArgs defines the arguments for find_files tool
type FindFilesArgs struct {
	Pattern string `json:"pattern"`
}

// FindFilesResult defines the results for find_files tool
type FindFilesResult struct {
	Files []string `json:"files"`
}

// SearchTextArgs defines the arguments for search_text tool
type SearchTextArgs struct {
	Query string `json:"query"`
	Glob  string `json:"glob"`
}

// SearchTextResult defines the results for search_text tool
type SearchTextResult struct {
	Evidence []types.Evidence `json:"evidence"`
}

// ReadFileArgs defines the arguments for read_file tool
type ReadFileArgs struct {
	Path string `json:"path"`
}

// ReadFileResult defines the results for read_file tool
type ReadFileResult struct {
	Content string `json:"content"`
}

func findFilesHandler(ctx tool.Context, args FindFilesArgs) (FindFilesResult, error) {
	log.Debug("find_files called", "pattern", args.Pattern)
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		err := fmt.Errorf("workspace not found in context")
		log.Error("find_files: no workspace in context", "error", err)
		return FindFilesResult{}, err
	}
	files, err := tools.FindFiles(ctx, workspace, args.Pattern)
	if err != nil {
		log.Error("find_files failed", "pattern", args.Pattern, "error", err)
		return FindFilesResult{}, err
	}
	log.Debug("find_files completed", "pattern", args.Pattern, "count", len(files))
	return FindFilesResult{Files: files}, nil
}

func searchTextHandler(ctx tool.Context, args SearchTextArgs) (SearchTextResult, error) {
	log.Debug("search_text called", "query", args.Query, "glob", args.Glob)
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		err := fmt.Errorf("workspace not found in context")
		log.Error("search_text: no workspace in context", "error", err)
		return SearchTextResult{}, err
	}
	evidence, _, err := tools.SearchWithRG(ctx, workspace, args.Query, args.Glob)
	if err != nil {
		log.Error("search_text failed", "query", args.Query, "error", err)
		return SearchTextResult{}, err
	}
	log.Debug("search_text completed", "query", args.Query, "count", len(evidence))
	return SearchTextResult{Evidence: evidence}, nil
}

func readFileHandler(ctx tool.Context, args ReadFileArgs) (ReadFileResult, error) {
	log.Debug("read_file called", "path", args.Path)
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		err := fmt.Errorf("workspace not found in context")
		log.Error("read_file: no workspace in context", "error", err)
		return ReadFileResult{}, err
	}
	content, err := tools.ReadFile(workspace, args.Path)
	if err != nil {
		log.Error("read_file failed", "path", args.Path, "error", err)
		return ReadFileResult{}, err
	}
	log.Debug("read_file completed", "path", args.Path, "chars", len(content))
	return ReadFileResult{Content: content}, nil
}

func (e *Engine) runAdkNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next_adk")
	defer span.End()

	log.Info("running ADK step",
		"task_id", task.ID,
		"step", task.StepCount+1,
		"max_steps", task.MaxSteps,
		"budget", task.ToolBudget,
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Info("step limit or budget reached",
			"task_id", task.ID,
			"step", task.StepCount,
			"max_steps", task.MaxSteps,
			"budget", task.ToolBudget,
		)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = finalAnswerForLimit(task, limitReasonStepOrToolBudget)
		}
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		return nil
	}

	r, err := e.getAdkRunner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to compile adk runner")
		log.Error("failed to get ADK runner", "task_id", task.ID, "error", err)
		return err
	}

	// Prepare execution context with workspace and task injection
	runCtx := context.WithValue(ctx, workspaceKey, task.Workspace)
	runCtx = context.WithValue(runCtx, taskKey, task)
	userMsg := genai.NewContentFromText(task.Goal, genai.RoleUser)

	log.Info("starting ADK execution session", "task_id", task.ID)
	var finalAnswer string
	for event, err := range r.Run(runCtx, "user", task.ID, userMsg, agent.RunConfig{}) {
		if err != nil {
			log.Error("ADK runner error", "task_id", task.ID, "error", err)
			return err
		}
		if event.IsFinalResponse() {
			if event.LLMResponse.Content != nil {
				var sb strings.Builder
				for _, part := range event.LLMResponse.Content.Parts {
					if part.Text != "" {
						sb.WriteString(part.Text)
					}
				}
				finalAnswer = sb.String()
			}
		}
	}

	log.Info("ADK execution session ended", "task_id", task.ID, "final_answer_len", len(finalAnswer))

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		ans := task.FinalAnswer
		if ans == "" {
			ans = finalAnswerForLimit(task, limitReasonStepOrToolBudget)
		}
		_ = SetTaskCompleted(task, ans)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
	} else if finalAnswer != "" {
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
	}

	return nil
}

func (e *Engine) getAdkRunner(ctx context.Context) (*runner.Runner, error) {
	e.adkOnce.Do(func() {
		log.Info("compiling ADK runner (first use)")
		r, err := e.compileAdkRunner(ctx)
		if err != nil {
			e.adkErr = err
			return
		}
		e.adkRunner = r
	})
	if e.adkErr != nil {
		return nil, e.adkErr
	}
	return e.adkRunner.(*runner.Runner), nil
}

func (e *Engine) compileAdkRunner(ctx context.Context) (*runner.Runner, error) {
	var llmModel model.LLM
	if e.AdkModel != nil {
		llmModel = e.AdkModel
	} else {
		// Set API Key
		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("GOOGLE_API_KEY")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY or GOOGLE_API_KEY is required for ADK mode")
		}

		modelName := os.Getenv("GEMINI_MODEL")
		if modelName == "" {
			modelName = "gemini-2.5-flash"
		}

		var err error
		llmModel, err = gemini.NewModel(ctx, modelName, &genai.ClientConfig{
			APIKey: apiKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create gemini model: %w", err)
		}
	}

	// Create tools
	findFilesTool, err := functiontool.New(functiontool.Config{
		Name:        "find_files",
		Description: "Find candidate files in the workspace matching a glob pattern (e.g. '*.txt', '*.md')",
	}, findFilesHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to create find_files tool: %w", err)
	}

	searchTextTool, err := functiontool.New(functiontool.Config{
		Name:        "search_text",
		Description: "Search text using ripgrep (rg) for a query string in the workspace, optionally filtering files by glob pattern",
	}, searchTextHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to create search_text tool: %w", err)
	}

	readFileTool, err := functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Read the content of a file (up to 4000 characters) given its relative path in the workspace",
	}, readFileHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to create read_file tool: %w", err)
	}

	// Interceptor callbacks to update trace and step count
	toolStartTime := make(map[string]time.Time)

	beforeToolCallback := func(toolCtx tool.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
		task, _ := toolCtx.Value(taskKey).(*types.Task)
		if task == nil {
			return nil, fmt.Errorf("task not found in context")
		}
		log.Debug("tool about to be invoked", "tool", t.Name(), "task_id", task.ID)
		if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
			err := fmt.Errorf("budget or step limit reached: step %d/%d, budget %d", task.StepCount, task.MaxSteps, task.ToolBudget)
			log.Warn("tool invocation blocked: budget/step limit",
				"tool", t.Name(),
				"task_id", task.ID,
				"step", task.StepCount,
				"max_steps", task.MaxSteps,
				"budget", task.ToolBudget,
			)
			return nil, err
		}
		toolStartTime[t.Name()] = time.Now()
		return args, nil
	}

	afterToolCallback := func(toolCtx tool.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
		task, _ := toolCtx.Value(taskKey).(*types.Task)
		if task == nil {
			return nil, fmt.Errorf("task not found in context")
		}
		var elapsed time.Duration
		if start, ok := toolStartTime[t.Name()]; ok {
			elapsed = time.Since(start)
			delete(toolStartTime, t.Name())
		}
		log.Debug("tool invocation completed",
			"tool", t.Name(),
			"task_id", task.ID,
			"elapsed_ms", elapsed.Milliseconds(),
			"error", err,
		)
		stepTrace := types.StepTrace{
			Step:   task.StepCount + 1,
			Goal:   task.Goal,
			Action: t.Name(),
		}

		if err != nil {
			stepTrace.Observation = fmt.Sprintf("Error: %v", err)
		} else {
			switch t.Name() {
			case "find_files":
				var files []string
				if filesVal, ok := result["files"].([]any); ok {
					for _, f := range filesVal {
						if s, ok := f.(string); ok {
							files = append(files, s)
						}
					}
				} else if filesVal, ok := result["files"].([]string); ok {
					files = filesVal
				}
				pattern, _ := args["pattern"].(string)
				stepTrace.Query = pattern
				stepTrace.Observation = fmt.Sprintf("found %d candidate files", len(files))

			case "search_text":
				var evidences []types.Evidence
				if evVal, ok := result["evidence"]; ok {
					if typedEv, ok := evVal.([]types.Evidence); ok {
						evidences = typedEv
					} else {
						b, _ := json.Marshal(evVal)
						_ = json.Unmarshal(b, &evidences)
					}
				}
				query, _ := args["query"].(string)
				stepTrace.Query = query
				stepTrace.Observation = fmt.Sprintf("found %d evidence items", len(evidences))
				stepTrace.Evidence = evidences

			case "read_file":
				content, _ := result["content"].(string)
				path, _ := args["path"].(string)
				stepTrace.Query = path
				stepTrace.Observation = "read file content: " + content

			default:
				stepTrace.Observation = fmt.Sprintf("Completed action %s with result: %+v", t.Name(), result)
			}
		}

		task.Trace = append(task.Trace, stepTrace)
		task.StepCount++
		task.ToolBudget--
		_ = SetTaskRunning(task)

		if e.Metrics != nil {
			e.Metrics.ObserveExecutor(elapsed, err, t.Name())
		}

		return result, nil
	}

	adkAgent, err := llmagent.New(llmagent.Config{
		Name:        "adk_orchestration_agent",
		Model:       llmModel,
		Description: "Orchestration agent to search files and retrieve information to solve the user task.",
		Instruction: `You are an autonomous search and retrieval agent.
Your goal is to solve the task using the provided tools.
Analyze the steps, look for candidate files, search for key text, read files, and stop when you have found the answer.
If you have found the answer, output the answer clearly. If the answer cannot be found after searching, say so.`,
		Tools: []tool.Tool{
			findFilesTool,
			searchTextTool,
			readFileTool,
		},
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{beforeToolCallback},
		AfterToolCallbacks:  []llmagent.AfterToolCallback{afterToolCallback},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create llm agent: %w", err)
	}

	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:           "ai-agent",
		Agent:             adkAgent,
		SessionService:    sessionService,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create runner: %w", err)
	}

	return r, nil
}
