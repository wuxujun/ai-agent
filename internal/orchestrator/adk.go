package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
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

const workspaceKey contextKey = "workspace"

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
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		return FindFilesResult{}, fmt.Errorf("workspace not found in context")
	}
	files, err := tools.FindFiles(workspace, args.Pattern)
	if err != nil {
		return FindFilesResult{}, err
	}
	return FindFilesResult{Files: files}, nil
}

func searchTextHandler(ctx tool.Context, args SearchTextArgs) (SearchTextResult, error) {
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		return SearchTextResult{}, fmt.Errorf("workspace not found in context")
	}
	evidence, _, err := tools.SearchWithRG(workspace, args.Query, args.Glob)
	if err != nil {
		return SearchTextResult{}, err
	}
	return SearchTextResult{Evidence: evidence}, nil
}

func readFileHandler(ctx tool.Context, args ReadFileArgs) (ReadFileResult, error) {
	workspace, _ := ctx.Value(workspaceKey).(string)
	if workspace == "" {
		return ReadFileResult{}, fmt.Errorf("workspace not found in context")
	}
	content, err := tools.ReadFile(workspace, args.Path)
	if err != nil {
		return ReadFileResult{}, err
	}
	return ReadFileResult{Content: content}, nil
}

func (e *Engine) runAdkNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next_adk")
	defer span.End()

	log.Printf("[ADK Engine] Running step %d/%d (budget: %d) for task %s", task.StepCount+1, task.MaxSteps, task.ToolBudget, task.ID)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Printf("[ADK Engine] Task %s reached step limit (%d/%d) or budget limit (%d)", task.ID, task.StepCount, task.MaxSteps, task.ToolBudget)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "stopped by budget or max steps"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		return nil
	}

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
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		if apiKey == "" {
			return fmt.Errorf("GEMINI_API_KEY, GOOGLE_API_KEY, or OPENAI_API_KEY is required for ADK mode")
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
			return fmt.Errorf("failed to create gemini model: %w", err)
		}
	}

	// Create tools
	findFilesTool, err := functiontool.New(functiontool.Config{
		Name:        "find_files",
		Description: "Find candidate files in the workspace matching a glob pattern (e.g. '*.txt', '*.md')",
	}, findFilesHandler)
	if err != nil {
		return fmt.Errorf("failed to create find_files tool: %w", err)
	}

	searchTextTool, err := functiontool.New(functiontool.Config{
		Name:        "search_text",
		Description: "Search text using ripgrep (rg) for a query string in the workspace, optionally filtering files by glob pattern",
	}, searchTextHandler)
	if err != nil {
		return fmt.Errorf("failed to create search_text tool: %w", err)
	}

	readFileTool, err := functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Read the content of a file (up to 4000 characters) given its relative path in the workspace",
	}, readFileHandler)
	if err != nil {
		return fmt.Errorf("failed to create read_file tool: %w", err)
	}

	// Interceptor callbacks to update trace and step count
	beforeToolCallback := func(toolCtx tool.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
		if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
			return nil, fmt.Errorf("budget or step limit reached: step %d/%d, budget %d", task.StepCount, task.MaxSteps, task.ToolBudget)
		}
		return args, nil
	}

	afterToolCallback := func(toolCtx tool.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
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
				if len(content) > 220 {
					content = content[:220]
				}
				stepTrace.Query = path
				stepTrace.Observation = "read file snippet: " + content

			default:
				stepTrace.Observation = fmt.Sprintf("Completed action %s with result: %+v", t.Name(), result)
			}
		}

		task.Trace = append(task.Trace, stepTrace)
		task.StepCount++
		task.ToolBudget--
		_ = SetTaskRunning(task)

		if e.Metrics != nil {
			e.Metrics.ObserveExecutor(time.Since(time.Now()), err, t.Name())
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
		return fmt.Errorf("failed to create llm agent: %w", err)
	}

	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:           "ai-agent",
		Agent:             adkAgent,
		SessionService:    sessionService,
		AutoCreateSession: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create runner: %w", err)
	}

	// Prepare execution context with workspace injection
	runCtx := context.WithValue(ctx, workspaceKey, task.Workspace)
	userMsg := genai.NewContentFromText(task.Goal, genai.RoleUser)

	var finalAnswer string
	for event, err := range r.Run(runCtx, "user", task.ID, userMsg, agent.RunConfig{}) {
		if err != nil {
			log.Printf("[ADK Engine Error] Runner run error: %v", err)
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

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		ans := task.FinalAnswer
		if ans == "" {
			ans = "stopped by budget or max steps"
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
