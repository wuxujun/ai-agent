package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

// writerSystemPrompt instructs the LLM to synthesise evidence into a final answer.
const writerSystemPrompt = `You are a research synthesis agent. You receive a user goal and a structured
collection of evidence gathered from local file searches. Your job is to write a clear,
accurate, and concise final answer.

Rules:
1. Base your answer ONLY on the provided evidence — do not hallucinate.
2. If the evidence directly answers the goal, set draft_confidence = "high".
3. If the evidence partially answers it, set draft_confidence = "medium".
4. If evidence is insufficient or absent, state this clearly and set draft_confidence = "low".
5. evidence_summary should be a brief (≤ 50 words) summary of the key evidence used.
6. final_answer should be complete and self-contained.`

// WriterAgent synthesises research evidence into a final answer using an LLM.
type WriterAgent struct {
	Config LLMConfig
}

func (w *WriterAgent) jsonSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"final_answer": map[string]any{
				"type":        "string",
				"description": "Complete answer to the user's goal based on the evidence",
			},
			"evidence_summary": map[string]any{
				"type":        "string",
				"description": "Brief summary of the key evidence used (max 50 words)",
			},
			"draft_confidence": map[string]any{
				"type":        "string",
				"enum":        []string{"high", "medium", "low"},
				"description": "Generation-time estimate used only to decide whether more research is needed",
			},
		},
		"required":             []string{"final_answer", "evidence_summary", "draft_confidence"},
		"additionalProperties": false,
	}
}

// Write calls the LLM to synthesise all gathered evidence into a WriterOutput.
func (w *WriterAgent) Write(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*WriterOutput, error) {
	log.Info("Synthesising answer", "goal", goal, "evidence_items", len(evidence))

	userPrompt := w.buildPrompt(goal, evidence, memories)

	var output WriterOutput
	cfg := w.Config
	if cfg.Provider == "" {
		cfg = LLMConfigForScene(config.LLMSceneMultiAgentWriter)
	}

	systemPrompt := writerSystemPrompt
	teamsCfg := GetTeamsConfig()
	activeTeam := teamsCfg.GetActiveTeam()
	if activeTeam.Writer.SystemPrompt != "" {
		systemPrompt = activeTeam.Writer.SystemPrompt
		log.Info("Using custom system prompt for WriterAgent", "team", teamsCfg.ActiveTeam, "agent_name", activeTeam.Writer.Name)
	} else {
		systemPrompt = promptmanager.GetManager().Get(ctx, "multiagent_writer_prompt", writerSystemPrompt)
	}
	if activeTeam.Writer.Provider != "" || activeTeam.Writer.Model != "" || activeTeam.Writer.LLMScene != "" {
		cfg = GetLLMConfig(activeTeam.Writer, config.LLMSceneMultiAgentWriter)
	}

	usage, err := callLLMJSON(ctx, cfg, systemPrompt, userPrompt, w.jsonSchema(), &output)
	if err != nil {
		return nil, fmt.Errorf("WriterAgent LLM call failed: %w", err)
	}
	output.TokenUsage = usage

	log.Info("Synthesis complete", "draft_confidence", output.resolvedDraftConfidence())
	return &output, nil
}

// buildPrompt assembles the evidence into a readable prompt for the WriterAgent.
func (w *WriterAgent) buildPrompt(goal string, evidence []StepEvidence, memories []types.Memory) string {
	var sb strings.Builder

	memorySection := formatMemories(memories)
	fmt.Fprintf(&sb, "Goal: %s%s\n\n", goal, memorySection)

	if len(evidence) == 0 {
		sb.WriteString("No evidence was gathered during the research phase.\n")
		return sb.String()
	}

	sb.WriteString("Evidence gathered during research:\n")
	for i, ev := range evidence {
		fmt.Fprintf(&sb, "\n── Step %d [%s / %s] ──\n", i+1, ev.StepID, ev.Action)
		fmt.Fprintf(&sb, "Description : %s\n", ev.StepDesc)
		fmt.Fprintf(&sb, "Observation : %s\n", ev.Observation)

		if len(ev.Evidence) > 0 {
			sb.WriteString("Evidence entries:\n")
			for _, e := range ev.Evidence {
				fmt.Fprintf(&sb, "  • File: %s\n", e.Path)
				for _, line := range e.Lines {
					// Truncate very long content to avoid overwhelming the context window.
					if len(line) > 2000 {
						line = line[:2000] + "\n  ... [truncated]"
					}
					fmt.Fprintf(&sb, "    %s\n", line)
				}
			}
		}
	}
	return sb.String()
}
