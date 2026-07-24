package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/skills"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	skillLog    = logger.Component("skills")
	skillTracer = otel.Tracer("ai-agent/skills")
)

// UseSkillTool loads the full instructions of an installed skill by name. It is
// the single entry point for the skill layer: instead of registering one tool
// per skill (which would bloat the merged planner schema's action enum and
// parameter set), every skill is reached through this one tool.
//
// Execute is read-only — it returns the SKILL.md body as an observation for the
// planner to follow on subsequent turns. Any side-effectful work the skill
// describes is still carried out by the existing tools (read_file,
// execute_code, write_file, ...), so their RiskLevel / approval gating remains
// fully in force. Keeping use_skill side-effect-free is a security invariant:
// loading a skill must never itself execute anything.
type UseSkillTool struct {
	Skills *skills.Registry
}

func (t *UseSkillTool) Name() string { return "use_skill" }

func (t *UseSkillTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }

func (t *UseSkillTool) Description() string {
	return "Load a skill's full instructions by name before performing a specialized task. " +
		"Choose a name from the listed available skills, then follow the returned instructions using the other tools."
}

func (t *UseSkillTool) Parameters() map[string]any {
	return map[string]any{
		"name": map[string]any{
			"type":        "string",
			"description": "Skill name to load; must exactly match one of the available skills.",
		},
	}
}

// Validate satisfies the optional planner.Validator contract (see the
// validate.go refactor in SKILL_INTEGRATION_DESIGN.md). It is a no-op-safe
// guard: until that refactor lands, ValidateDecision's default switch case
// already lets use_skill through, so this method is purely additive.
func (t *UseSkillTool) Validate(params map[string]any) error {
	name, _ := params["name"].(string)
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("use_skill requires non-empty name")
	}
	return nil
}

func (t *UseSkillTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	name, _ := params["name"].(string)
	name = strings.TrimSpace(name)
	mode := strings.TrimSpace(config.Get().Orchestrator.Mode)

	ctx, span := skillTracer.Start(ctx, "skill.use",
		trace.WithAttributes(
			attribute.String("agent.skill.name", name),
			attribute.String("agent.orchestrator.mode", mode),
		),
	)
	defer span.End()

	if t.Skills == nil {
		err := fmt.Errorf("use_skill: skill registry not configured")
		recordSkillUseFailure(ctx, span, name, mode, err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("agent.skill.registry_count", len(t.Skills.List())))
	if name == "" {
		err := fmt.Errorf("use_skill requires non-empty name")
		recordSkillUseFailure(ctx, span, name, mode, err)
		return nil, err
	}

	skill, ok := t.Skills.Get(name)
	if !ok {
		// Surface the available names so the planner can self-correct next turn.
		available := make([]string, 0)
		for _, s := range t.Skills.List() {
			available = append(available, s.Name)
		}
		err := fmt.Errorf("use_skill: unknown skill %q; available: %s", name, strings.Join(available, ", "))
		recordSkillUseFailure(ctx, span, name, mode, err)
		return nil, err
	}

	body, resources, err := t.Skills.Body(name)
	if err != nil {
		err = fmt.Errorf("use_skill: %w", err)
		recordSkillUseFailure(ctx, span, name, mode, err)
		return nil, err
	}
	span.SetAttributes(
		attribute.Bool("agent.skill.loaded", true),
		attribute.Int("agent.skill.resource_count", len(resources)),
		attribute.Int("agent.skill.allowed_tool_count", len(skill.AllowedTools)),
	)
	span.SetStatus(codes.Ok, "skill loaded")
	skillLog.InfoContext(ctx, "skill invoked",
		appendTraceFields(span,
			"skill", name,
			"mode", mode,
			"resource_count", len(resources),
			"allowed_tool_count", len(skill.AllowedTools),
		)...,
	)

	var b strings.Builder
	fmt.Fprintf(&b, "Loaded skill %q. Follow these instructions using the available tools.\n\n", name)
	if len(skill.AllowedTools) > 0 {
		fmt.Fprintf(&b, "Allowed tools for this skill: %s\n\n", strings.Join(skill.AllowedTools, ", "))
	}
	if len(resources) > 0 {
		fmt.Fprintf(&b, "Bundled resources (read with read_file relative to %s): %s\n\n", skill.Dir, strings.Join(resources, ", "))
	}
	b.WriteString("--- SKILL INSTRUCTIONS ---\n")
	b.WriteString(body)

	return &ToolResult{
		Query:       "use_skill:" + name,
		Observation: b.String(),
	}, nil
}

func recordSkillUseFailure(ctx context.Context, span trace.Span, name, mode string, err error) {
	span.RecordError(err)
	span.SetAttributes(attribute.Bool("agent.skill.loaded", false))
	span.SetStatus(codes.Error, "skill load failed")
	skillLog.WarnContext(ctx, "skill invocation failed",
		appendTraceFields(span,
			"skill", name,
			"mode", mode,
			"error", err,
		)...,
	)
}

func appendTraceFields(span trace.Span, fields ...any) []any {
	spanContext := span.SpanContext()
	if !spanContext.IsValid() {
		return fields
	}
	return append(fields,
		"trace_id", spanContext.TraceID().String(),
		"span_id", spanContext.SpanID().String(),
	)
}

// RegisterUseSkill wires the use_skill tool into the default registry. Unlike
// other tools (which self-register in init()), use_skill needs a live
// *skills.Registry, so it must be registered explicitly from main() AFTER the
// skill registry is built and BEFORE the planner first compiles its schema.
//
// Registering here still keeps the three-way invariant intact: use_skill goes
// through the same DefaultRegistry that PlannerDecisionSchema,
// PlannerDecisionGenAISchema and ValidateDecision all derive from.
func RegisterUseSkill(reg *skills.Registry) {
	Register(&UseSkillTool{Skills: reg})
}
