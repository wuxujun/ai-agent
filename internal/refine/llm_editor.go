package refine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

var refineLog = logger.Component("refine")

const (
	// EditorPromptName is the Langfuse prompt name for the harness editor.
	EditorPromptName = "harness-editor"
	// EditorPromptLabel is the non-production label for editor prompts.
	// Editor prompts must never pollute production labels.
	EditorPromptLabel = "harness-candidate"
)

// defaultEditorSystemPrompt is the embedded fallback for the editor system
// prompt. When Langfuse prompt management is enabled it is seeded under the
// non-production EditorPromptLabel and resolved (pinned) from there; the
// embedded text is only the fallback.
const defaultEditorSystemPrompt = `You are a harness engineer for a multi-agent AI system. Your job is to propose small, safe improvements to ONE team's harness configuration.

EDITABLE SURFACE (you may ONLY touch these targets):
- teams.<team>.planner|researcher|writer|executor|critic|verifier.system_prompt   (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.prompt_name      (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.langfuse_prompt  (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.prompt_label     (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.prompt_version   (op: set, integer)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.provider        (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.model            (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.llm_scene        (op: set)
- teams.<team>.planner|researcher|writer|executor|critic|verifier.tools            (op: add-tools or remove-tools; after is a JSON array of tool names)
- teams.<team>.routing.reviewed_min_plan_steps                                     (op: set, integer)
- teams.<team>.routing.reviewed_min_remaining_llm_calls                            (op: set, integer)
- teams.<team>.routing.reviewed_min_remaining_tokens                               (op: set, integer)

RULES:
- Propose up to K general improvements (good for any team) and up to K domain improvements (specific to this team's workflow).
- Every edit needs target, op, after, and a one-sentence reason. For op=set, also give before = the exact current value shown to you.
- Prefer prompt and routing tweaks over tool changes. Never widen tool access without a clear reason tied to the team's work.
- Do not invent field names, roles, or ops outside the editable surface. Invalid edits are discarded automatically.
- Be concrete and minimal: one behavior change per edit.

OUTPUT: JSON only, matching the provided schema.`

// LLMEditor is the production harness editor: it asks a model to propose
// candidate edits. Credentials come from the resolved LLM scene (scene
// harness_editor, falling back to the provider default); nothing is read
// from chat or the environment beyond the standard config.
type LLMEditor struct {
	// Scene overrides the LLM scene (default config.LLMSceneHarnessEditor).
	Scene string
	// LLMConfig overrides the resolved scene config (tests, mainly).
	LLMConfig *llm.Config
	// SystemPrompt overrides the promptmanager/embedded system prompt.
	SystemPrompt string
	// MaxCostUSD caps the estimated cost of the proposal call; 0 disables.
	// Exceeding it fails the proposal.
	MaxCostUSD float64
}

func (e LLMEditor) sceneName() string {
	if strings.TrimSpace(e.Scene) != "" {
		return e.Scene
	}
	return config.LLMSceneHarnessEditor
}

func (e LLMEditor) llmConfig() llm.Config {
	if e.LLMConfig != nil {
		return *e.LLMConfig
	}
	return llm.ConfigForScene(e.sceneName())
}

// Propose asks the model for up to k general and up to k domain candidates.
// Invalid edits are dropped (counted in the debug log); the rest are
// returned with the call's token usage for cost tracking.
func (e LLMEditor) Propose(ctx context.Context, base harness.Snapshot, team string, k int, lessons []improvement.Lesson) ([]harness.EditSet, []harness.EditSet, types.TokenUsage, error) {
	if _, ok := base.Teams.Teams[team]; !ok {
		return nil, nil, types.TokenUsage{}, fmt.Errorf("team %q not found in snapshot", team)
	}
	if k < 1 {
		return nil, nil, types.TokenUsage{}, fmt.Errorf("k must be >= 1")
	}
	cfg := e.llmConfig()
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, nil, types.TokenUsage{}, fmt.Errorf("llm editor not configured: no model for scene %q (set llm.scenes.%s.model or the provider default model)", e.sceneName(), e.sceneName())
	}
	systemPrompt := e.resolveSystemPrompt(ctx)
	userPrompt := buildEditorPrompt(base, team, k, lessons)

	var output llmProposal
	usage, err := llm.CallJSON(ctx, cfg, systemPrompt, truncatePrompt(userPrompt, 60000), editorSchema(k), &output)
	if err != nil {
		return nil, nil, usage, fmt.Errorf("llm editor call: %w", err)
	}
	if max := e.MaxCostUSD; max > 0 {
		if cost := llm.EstimateCostUSD(cfg, usage); cost > max {
			return nil, nil, usage, fmt.Errorf("llm editor cost $%.4f exceeded ceiling $%.4f", cost, max)
		}
	}
	general, droppedG := toEditSets(base, "llm-editor:general", output.General)
	domain, droppedD := toEditSets(base, "llm-editor:domain", output.Domain)
	if droppedG+droppedD > 0 {
		refineLog.Debug("llm editor dropped invalid edits", "dropped", droppedG+droppedD)
	}
	return capK(general, k), capK(domain, k), usage, nil
}

// EstimateCost returns the estimated USD cost of a proposal call under the
// editor's resolved scene pricing.
func (e LLMEditor) EstimateCost(usage types.TokenUsage) float64 {
	return llm.EstimateCostUSD(e.llmConfig(), usage)
}

// resolveSystemPrompt seeds the editor prompt under the non-production label
// when Langfuse management is enabled, then resolves (and pins) it.
// Failures fall back to the embedded default.
func (e LLMEditor) resolveSystemPrompt(ctx context.Context) string {
	if strings.TrimSpace(e.SystemPrompt) != "" {
		return e.SystemPrompt
	}
	pm := promptmanager.GetManager()
	_, _, _ = pm.EnsureTextPrompt(ctx, promptmanager.Seed{
		Name:     EditorPromptName,
		Content:  defaultEditorSystemPrompt,
		Selector: promptmanager.Selector{Label: EditorPromptLabel},
	})
	resolved, err := pm.ResolvePinned(ctx, EditorPromptName,
		promptmanager.Selector{Label: EditorPromptLabel}, defaultEditorSystemPrompt)
	if err != nil || strings.TrimSpace(resolved.Content) == "" {
		return defaultEditorSystemPrompt
	}
	return resolved.Content
}

// llmEdit is one model-proposed edit.
type llmEdit struct {
	Target string `json:"target"`
	Op     string `json:"op"`
	Before string `json:"before"`
	After  string `json:"after"`
	Reason string `json:"reason"`
}

// llmProposal is the model's structured output.
type llmProposal struct {
	General []llmEdit `json:"general"`
	Domain  []llmEdit `json:"domain"`
}

// editorSchema constrains the model's output to valid edit shapes.
func editorSchema(k int) map[string]any {
	edit := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"target": map[string]any{"type": "string"},
			"op":     map[string]any{"type": "string", "enum": []string{"set", "add-tools", "remove-tools"}},
			"before": map[string]any{"type": "string"},
			"after":  map[string]any{"type": "string"},
			"reason": map[string]any{"type": "string", "maxLength": 500},
		},
		"required": []string{"target", "op", "after", "reason"},
	}
	arr := map[string]any{"type": "array", "maxItems": k, "items": edit}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"general": arr,
			"domain":  arr,
		},
		"required": []string{"general", "domain"},
	}
}

// toEditSets converts model edits to validated EditSets, one candidate per
// edit (matching the mock editor's granularity). Invalid edits are dropped;
// the drop count is returned for reporting.
func toEditSets(base harness.Snapshot, author string, edits []llmEdit) ([]harness.EditSet, int) {
	var out []harness.EditSet
	dropped := 0
	for _, e := range edits {
		es := harness.EditSet{
			SnapshotID: base.ID,
			Author:     author,
			Edits: []harness.Edit{{
				Target: e.Target, Op: e.Op, Before: e.Before, After: e.After, Reason: e.Reason,
			}},
		}
		if err := harness.Validate(es); err != nil {
			dropped++
			continue
		}
		out = append(out, es)
	}
	return out, dropped
}

func capK(sets []harness.EditSet, k int) []harness.EditSet {
	if len(sets) > k {
		return sets[:k]
	}
	return sets
}

// buildEditorPrompt renders the team config and prior lessons as the
// model's working context.
func buildEditorPrompt(base harness.Snapshot, team string, k int, lessons []improvement.Lesson) string {
	var sb strings.Builder
	teamJSON, _ := json.MarshalIndent(base.Teams.Teams[team], "", "  ")
	fmt.Fprintf(&sb, "Team %q current harness config:\n%s\n\n", team, truncatePrompt(string(teamJSON), 12000))
	if len(lessons) > 0 {
		sb.WriteString("Prior adopted improvements for this team (experience: prefer edits consistent with what worked, avoid repeating what was adopted):\n")
		for i, l := range lessons {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&sb, "- %s (confidence %.2f): ", l.ContrastDelta, l.Confidence)
			for j, e := range l.Edits {
				if j > 0 {
					sb.WriteString("; ")
				}
				fmt.Fprintf(&sb, "%s %s -> %s", e.Op, e.Target, truncatePrompt(e.After, 80))
			}
			if len(l.Evidence) > 0 {
				fmt.Fprintf(&sb, " [%s]", truncatePrompt(l.Evidence[0].Summary, 120))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "Propose up to %d general improvements and up to %d domain-specific improvements for team %q. Return JSON only.", k, k, team)
	return sb.String()
}

func truncatePrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
