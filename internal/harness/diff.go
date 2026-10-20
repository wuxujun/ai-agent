package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagent"
)

// Whitelisted editable surface. Anything not listed here cannot be addressed
// by an Edit and is rejected by Validate.
var (
	roleNames = []string{"planner", "critic", "executor", "verifier", "researcher", "writer"}

	// roleFields maps target field name -> kind ("string", "int", "tools").
	roleFields = map[string]string{
		"system_prompt":   "string",
		"prompt_name":     "string",
		"langfuse_prompt": "string",
		"prompt_label":    "string",
		"prompt_version":  "int",
		"provider":        "string",
		"model":           "string",
		"llm_scene":       "string",
		"tools":           "tools",
	}
	roleFieldOrder = []string{
		"system_prompt", "prompt_name", "langfuse_prompt", "prompt_label",
		"prompt_version", "provider", "model", "llm_scene", "tools",
	}

	// routingFields maps target field name -> kind. Note: the
	// allow_research_high_risk_tools flag is deliberately excluded; widening
	// the tool surface needs human approval, not an automated edit.
	routingFields = map[string]string{
		"reviewed_min_plan_steps":          "int",
		"reviewed_min_remaining_llm_calls": "int",
		"reviewed_min_remaining_tokens":    "int",
	}
	routingFieldOrder = []string{
		"reviewed_min_plan_steps",
		"reviewed_min_remaining_llm_calls",
		"reviewed_min_remaining_tokens",
	}
)

// parsedTarget is a validated, structured edit target.
type parsedTarget struct {
	team  string
	role  string // "" when routing target
	route bool
	field string
	kind  string // "string" | "int" | "tools"
}

func parseTarget(target string) (parsedTarget, error) {
	// NOTE: team names containing '.' are not supported by this grammar.
	parts := strings.Split(target, ".")
	if len(parts) != 4 || parts[0] != "teams" {
		return parsedTarget{}, fmt.Errorf("invalid target %q: want teams.<team>.<role|routing>.<field>", target)
	}
	pt := parsedTarget{team: parts[1]}
	switch parts[2] {
	case "routing":
		pt.route = true
		kind, ok := routingFields[parts[3]]
		if !ok {
			return parsedTarget{}, fmt.Errorf("target %q: routing field %q is not editable", target, parts[3])
		}
		pt.field, pt.kind = parts[3], kind
	default:
		if !slices.Contains(roleNames, parts[2]) {
			return parsedTarget{}, fmt.Errorf("target %q: unknown role %q", target, parts[2])
		}
		kind, ok := roleFields[parts[3]]
		if !ok {
			return parsedTarget{}, fmt.Errorf("target %q: role field %q is not editable", target, parts[3])
		}
		pt.role, pt.field, pt.kind = parts[2], parts[3], kind
	}
	return pt, nil
}

func roleConfig(cfg *multiagent.TeamConfig, role string) *multiagent.AgentConfig {
	switch role {
	case "planner":
		return &cfg.Planner
	case "critic":
		return &cfg.Critic
	case "executor":
		return &cfg.Executor
	case "verifier":
		return &cfg.Verifier
	case "researcher":
		return &cfg.Researcher
	case "writer":
		return &cfg.Writer
	}
	return nil
}

// canonicalFieldValue renders a field in its canonical string form:
// scalars as-is, prompt_version as decimal, tools as a JSON array.
func canonicalFieldValue(agent *multiagent.AgentConfig, field string) (string, error) {
	switch field {
	case "system_prompt":
		return agent.SystemPrompt, nil
	case "prompt_name":
		return agent.PromptName, nil
	case "langfuse_prompt":
		return agent.LangfusePrompt, nil
	case "prompt_label":
		return agent.PromptLabel, nil
	case "prompt_version":
		return strconv.Itoa(agent.PromptVersion), nil
	case "provider":
		return agent.Provider, nil
	case "model":
		return agent.Model, nil
	case "llm_scene":
		return agent.LLMScene, nil
	case "tools":
		tools := agent.Tools
		if tools == nil {
			tools = []string{}
		}
		data, err := json.Marshal(tools)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return "", fmt.Errorf("unknown role field %q", field)
}

func canonicalRoutingValue(cfg *multiagent.TeamConfig, field string) (string, error) {
	switch field {
	case "reviewed_min_plan_steps":
		return strconv.Itoa(cfg.Routing.ReviewedMinPlanSteps), nil
	case "reviewed_min_remaining_llm_calls":
		return strconv.Itoa(cfg.Routing.ReviewedMinRemainingLLMCalls), nil
	case "reviewed_min_remaining_tokens":
		return strconv.Itoa(cfg.Routing.ReviewedMinRemainingTokens), nil
	}
	return "", fmt.Errorf("unknown routing field %q", field)
}

// Diff computes the typed edits that transform a into b. Every emitted edit
// uses Op "set" with canonical values, so Diff(a, Apply(a, es)) round-trips.
func Diff(a, b Snapshot) ([]Edit, error) {
	var edits []Edit
	teamSet := map[string]bool{}
	for _, t := range sortedKeys(a.Teams.Teams) {
		teamSet[t] = true
	}
	for _, t := range sortedKeys(b.Teams.Teams) {
		teamSet[t] = true
	}
	for _, team := range sortedKeys(teamSet) {
		ac, aok := a.Teams.Teams[team]
		bc, bok := b.Teams.Teams[team]
		if !aok || !bok {
			return nil, fmt.Errorf("diff: team %q present in only one snapshot (team add/remove not supported)", team)
		}
		for _, role := range roleNames {
			ar, br := roleConfig(&ac, role), roleConfig(&bc, role)
			for _, field := range roleFieldOrder {
				av, err := canonicalFieldValue(ar, field)
				if err != nil {
					return nil, err
				}
				bv, err := canonicalFieldValue(br, field)
				if err != nil {
					return nil, err
				}
				if av != bv {
					edits = append(edits, Edit{
						Target: fmt.Sprintf("teams.%s.%s.%s", team, role, field),
						Op:     "set",
						Before: av,
						After:  bv,
					})
				}
			}
		}
		for _, field := range routingFieldOrder {
			av, err := canonicalRoutingValue(&ac, field)
			if err != nil {
				return nil, err
			}
			bv, err := canonicalRoutingValue(&bc, field)
			if err != nil {
				return nil, err
			}
			if av != bv {
				edits = append(edits, Edit{
					Target: fmt.Sprintf("teams.%s.routing.%s", team, field),
					Op:     "set",
					Before: av,
					After:  bv,
				})
			}
		}
	}
	return edits, nil
}

// Validate checks an EditSet without applying it. All edits are validated
// before any is applied, so Apply is atomic with respect to validation.
func Validate(es EditSet) error {
	for i, e := range es.Edits {
		pt, err := parseTarget(e.Target)
		if err != nil {
			return fmt.Errorf("edit %d: %w", i, err)
		}
		switch e.Op {
		case "set":
			if err := checkSetValue(pt, e.After); err != nil {
				return fmt.Errorf("edit %d (%s): %w", i, e.Target, err)
			}
		case "add-tools", "remove-tools":
			if pt.kind != "tools" {
				return fmt.Errorf("edit %d (%s): op %q only valid on tools fields", i, e.Target, e.Op)
			}
			if _, err := parseToolList(e.After); err != nil {
				return fmt.Errorf("edit %d (%s): %w", i, e.Target, err)
			}
		default:
			return fmt.Errorf("edit %d (%s): unknown op %q", i, e.Target, e.Op)
		}
	}
	return nil
}

func checkSetValue(pt parsedTarget, after string) error {
	switch pt.kind {
	case "int":
		if _, err := strconv.Atoi(strings.TrimSpace(after)); err != nil {
			return fmt.Errorf("want integer, got %q", after)
		}
	case "tools":
		if _, err := parseToolList(after); err != nil {
			return err
		}
	}
	return nil
}

func parseToolList(s string) ([]string, error) {
	var tools []string
	if err := json.Unmarshal([]byte(s), &tools); err != nil {
		return nil, fmt.Errorf("want JSON array of tool names, got %q", s)
	}
	return tools, nil
}

// Apply validates es and returns the candidate snapshot. The base snapshot is
// never mutated. If es.SnapshotID is set it must match base.ID, guarding
// against applying edits to the wrong base.
func Apply(base Snapshot, es EditSet) (Snapshot, error) {
	if err := Validate(es); err != nil {
		return Snapshot{}, err
	}
	if es.SnapshotID != "" && es.SnapshotID != base.ID {
		return Snapshot{}, fmt.Errorf("edit set targets snapshot %q, base is %q", es.SnapshotID, base.ID)
	}

	// Deep copy via JSON round-trip (TeamsConfig carries json tags).
	raw, err := json.Marshal(base.Teams)
	if err != nil {
		return Snapshot{}, fmt.Errorf("copy base teams: %w", err)
	}
	var teams multiagent.TeamsConfig
	if err := json.Unmarshal(raw, &teams); err != nil {
		return Snapshot{}, fmt.Errorf("copy base teams: %w", err)
	}

	for i, e := range es.Edits {
		pt, _ := parseTarget(e.Target) // validated above
		team, ok := teams.Teams[pt.team]
		if !ok {
			return Snapshot{}, fmt.Errorf("edit %d: team %q not found", i, pt.team)
		}
		if err := applyOne(&team, pt, e); err != nil {
			return Snapshot{}, fmt.Errorf("edit %d: %w", i, err)
		}
		teams.Teams[pt.team] = team
	}

	// Skill digests carry over: edits never touch skills/ in Phase 0.
	digests := map[string]string{}
	for k, v := range base.SkillDigests {
		digests[k] = v
	}
	id, err := digestSnapshot(teams, digests)
	if err != nil {
		return Snapshot{}, err
	}
	source := base.Source
	if es.Author != "" {
		source = "applied:" + es.Author
	}
	return Snapshot{
		ID:           id,
		CreatedAt:    time.Now().UTC(),
		Source:       source,
		Teams:        teams,
		SkillDigests: digests,
	}, nil
}

func applyOne(team *multiagent.TeamConfig, pt parsedTarget, e Edit) error {
	if pt.route {
		return applyRouting(team, pt, e)
	}
	agent := roleConfig(team, pt.role)
	if agent == nil {
		return fmt.Errorf("unknown role %q", pt.role)
	}
	switch e.Op {
	case "set":
		return setRoleField(agent, pt.field, e.After)
	case "add-tools", "remove-tools":
		return mutateTools(agent, e.Op, e.After)
	}
	return fmt.Errorf("unknown op %q", e.Op)
}

func setRoleField(agent *multiagent.AgentConfig, field, after string) error {
	switch field {
	case "system_prompt":
		agent.SystemPrompt = after
	case "prompt_name":
		agent.PromptName = after
	case "langfuse_prompt":
		agent.LangfusePrompt = after
	case "prompt_label":
		agent.PromptLabel = after
	case "prompt_version":
		v, _ := strconv.Atoi(strings.TrimSpace(after)) // validated
		agent.PromptVersion = v
	case "provider":
		agent.Provider = after
	case "model":
		agent.Model = after
	case "llm_scene":
		agent.LLMScene = after
	case "tools":
		tools, _ := parseToolList(after) // validated
		agent.Tools = tools
	default:
		return fmt.Errorf("unknown role field %q", field)
	}
	return nil
}

func applyRouting(team *multiagent.TeamConfig, pt parsedTarget, e Edit) error {
	if e.Op != "set" {
		return fmt.Errorf("op %q not supported on routing fields", e.Op)
	}
	v, _ := strconv.Atoi(strings.TrimSpace(e.After)) // validated
	switch pt.field {
	case "reviewed_min_plan_steps":
		team.Routing.ReviewedMinPlanSteps = v
	case "reviewed_min_remaining_llm_calls":
		team.Routing.ReviewedMinRemainingLLMCalls = v
	case "reviewed_min_remaining_tokens":
		team.Routing.ReviewedMinRemainingTokens = v
	default:
		return fmt.Errorf("unknown routing field %q", pt.field)
	}
	return nil
}

func mutateTools(agent *multiagent.AgentConfig, op, after string) error {
	delta, _ := parseToolList(after) // validated
	cur := slices.Clone(agent.Tools)
	switch op {
	case "add-tools":
		for _, t := range delta {
			if !slices.Contains(cur, t) {
				cur = append(cur, t)
			}
		}
	case "remove-tools":
		keep := cur[:0]
		for _, t := range cur {
			if !slices.Contains(delta, t) {
				keep = append(keep, t)
			}
		}
		cur = keep
	}
	agent.Tools = cur
	return nil
}
