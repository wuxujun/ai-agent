package orchestrator

import (
	"context"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/refine"
	"github.com/wuxujun/ai-agent/internal/types"
)

// refinePlan is the per-task online-refinement decision (Phase 3). It is
// computed before Coordinator.Run and drives two hooks:
//
//  1. Overlay (serving): a canary-bucketed task runs with the newest adopted
//     improvement lesson applied to its in-memory team config.
//  2. Assessment (learning): after a freshly-bucketed task completes, an
//     async worker records the task outcome plus editor-proposed candidates
//     as an Assessment for operators to promote to offline probe runs.
//
// Refinement never fails or delays a task: every error path degrades to the
// base harness and logs.
type refinePlan struct {
	team     string
	teamsCfg *multiagent.TeamsConfig
	gate     multiagent.RefineGate
	// lesson to apply; nil when the canary selected the task but no adopted
	// lesson exists yet (assessment still runs: this is how the harness
	// learns from traffic before the first lesson exists).
	lesson *improvement.Lesson
	// persisted is true when the lesson comes from a previous run's trace
	// record (resume path) rather than a fresh canary bucketing.
	persisted bool
	// bucket/percent feed the trace record.
	bucket  int
	percent int
	// assessable is true when this run freshly bucketed the task.
	assessable bool
}

// planRefinement resolves the refinement plan for a task. It never mutates
// the task.
func planRefinement(ctx context.Context, task *types.Task) refinePlan {
	var plan refinePlan
	rc := config.Get().MultiAgent.HarnessRefine
	if !rc.Enabled || rc.CanaryPercent <= 0 {
		return plan
	}
	if task == nil || strings.TrimSpace(task.ID) == "" {
		return plan
	}
	lessonsDir := strings.TrimSpace(rc.LessonsDir)
	if lessonsDir == "" {
		return plan
	}
	team := multiAgentTeamName(task)
	plan.team = team
	plan.percent = rc.CanaryPercent
	plan.bucket = multiagent.RefineBucket(team, task.ID)

	// Resume path: re-apply the previously selected lesson so the
	// team-config digest stays stable across runs.
	if sel, ok := multiagent.PersistedRefineSelection(task, team); ok {
		lesson, found, err := improvement.NewStore(lessonsDir).Get(ctx, sel.LessonID)
		if err != nil || !found {
			log.Warn("refinement: persisted lesson unavailable, running base harness",
				"task_id", task.ID, "lesson_id", sel.LessonID, "error", err)
			return refinePlan{}
		}
		plan.lesson = &lesson
		plan.persisted = true
		plan.bucket = sel.Bucket
		plan.percent = sel.Percent
		return plan
	}

	teamsCfg, err := multiagent.TeamsConfigForTask(task)
	if err != nil {
		log.Warn("refinement: teams config unavailable, running base harness",
			"task_id", task.ID, "error", err)
		return refinePlan{}
	}
	teamCfg := teamsCfg.GetActiveTeam()
	plan.teamsCfg = teamsCfg
	plan.gate = multiagent.RefineGate{
		Enabled:    true,
		Percent:    rc.CanaryPercent,
		DAGPercent: teamCfg.DAGCanaryPercent,
		Runtime:    teamsCfg.ActiveRuntime(),
		Team:       team,
		TaskID:     task.ID,
	}
	if !plan.gate.Bucketed() {
		return refinePlan{}
	}
	plan.assessable = true
	lessons, err := improvement.NewStore(lessonsDir).List(ctx, team, 1)
	if err != nil {
		log.Warn("refinement: list lessons, assessing without overlay",
			"task_id", task.ID, "error", err)
		return plan
	}
	if len(lessons) > 0 {
		plan.lesson = &lessons[0]
	}
	return plan
}

// applyOverlay registers the planned lesson's refined team config for this
// task and records the decision in the task trace. It returns the cleanup
// func the caller must defer and the harness digest the task effectively
// runs with. Failures degrade to the base harness.
func (p refinePlan) applyOverlay(ctx context.Context, task *types.Task, skillsRoot string) (func(), string) {
	noop := func() {}
	if p.lesson == nil || task == nil {
		return noop, ""
	}
	teamsCfg := p.teamsCfg
	if teamsCfg == nil {
		var err error
		teamsCfg, err = multiagent.TeamsConfigForTask(task)
		if err != nil {
			log.Warn("refinement: teams config unavailable, running base harness",
				"task_id", task.ID, "error", err)
			return noop, ""
		}
	}
	digests, err := harness.SkillDigests(skillsRoot)
	if err != nil {
		log.Warn("refinement: skill digests unavailable, running base harness",
			"task_id", task.ID, "error", err)
		return noop, ""
	}
	snap, err := harness.NewSnapshot(*teamsCfg, digests, "refine:overlay")
	if err != nil {
		log.Warn("refinement: snapshot failed, running base harness",
			"task_id", task.ID, "error", err)
		return noop, ""
	}
	refined, err := harness.Apply(snap, harness.EditSet{
		Edits:  p.lesson.Edits,
		Author: "refine:overlay",
	})
	if err != nil {
		log.Warn("refinement: apply lesson failed, running base harness",
			"task_id", task.ID, "lesson_id", p.lesson.ID, "error", err)
		return noop, ""
	}
	teamCfg, ok := refined.Teams.Teams[p.team]
	if !ok {
		log.Warn("refinement: refined team missing, running base harness",
			"task_id", task.ID, "team", p.team)
		return noop, ""
	}
	multiagent.SetTeamOverlay(task.ID, p.team, teamCfg)
	if !p.persisted {
		multiagent.AppendRefineSelection(task, p.team, p.lesson.ID, snap.ID, refined.ID,
			p.bucket, p.percent, false)
	}
	log.Info("refinement: applied lesson overlay",
		"task_id", task.ID, "team", p.team, "lesson_id", shortID(p.lesson.ID),
		"harness_before", shortID(snap.ID), "harness_after", shortID(refined.ID),
		"persisted", p.persisted)
	return func() { multiagent.ClearTeamOverlay(task.ID) }, refined.ID
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// assessSnapshot carries the task fields the async assessment needs; it is
// captured synchronously because the task may be mutated after Run returns.
type assessSnapshot struct {
	taskID        string
	team          string
	status        string
	steps         int
	errorSteps    int
	harnessDigest string
	teamsCfg      *multiagent.TeamsConfig
}

// assessAsync records a harnessing-level observation after a freshly-bucketed
// task completes. It runs in a goroutine and never affects the task result.
func (p refinePlan) assessAsync(task *types.Task, rc config.HarnessRefineConfig, skillsRoot string, harnessDigest string) {
	if !p.assessable || task == nil || !types.IsTerminalTaskStatus(task.Status) {
		return
	}
	// Resolve while the task overlay is still installed. The async worker may
	// start after the caller clears it or reloads the base teams configuration.
	teamsCfg, err := multiagent.TeamsConfigForTask(task)
	if err != nil {
		log.Warn("refinement: assessment skipped, teams config unavailable",
			"task_id", task.ID, "error", err)
		return
	}
	snap := assessSnapshot{
		taskID:        task.ID,
		team:          p.team,
		status:        string(task.Status),
		steps:         task.StepCount,
		errorSteps:    countErrorSteps(task),
		harnessDigest: harnessDigest,
		teamsCfg:      teamsCfg,
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("refinement: assessment panicked", "task_id", snap.taskID, "panic", r)
			}
		}()
		runAssessment(context.Background(), snap, rc, skillsRoot)
	}()
}

func countErrorSteps(task *types.Task) int {
	n := 0
	for _, tr := range task.Trace {
		if strings.TrimSpace(tr.Error) != "" {
			n++
		}
	}
	return n
}

// buildAssessmentEditor selects the editor for online assessments from the
// refinement config. "mock" (default) costs nothing; "llm" spends one model
// call per canary-bucketed task, capped by AssessmentMaxCostUSD.
func buildAssessmentEditor(rc config.HarnessRefineConfig) refine.Editor {
	if strings.EqualFold(strings.TrimSpace(rc.AssessmentEditor), "llm") {
		return refine.LLMEditor{MaxCostUSD: rc.AssessmentMaxCostUSD}
	}
	return refine.MockEditor{}
}

// runAssessment proposes editor candidates for the harness the task ran with
// and records the outcome as an Assessment. Operators promote interesting
// assessments to offline probe runs (harness-refine run), which may produce
// adopted Lessons.
func runAssessment(ctx context.Context, snap assessSnapshot, rc config.HarnessRefineConfig, skillsRoot string) {
	if snap.teamsCfg == nil {
		log.Warn("refinement: assessment skipped, teams snapshot unavailable", "task_id", snap.taskID)
		return
	}
	digests, err := harness.SkillDigests(skillsRoot)
	if err != nil {
		log.Warn("refinement: assessment skipped, skill digests unavailable",
			"task_id", snap.taskID, "error", err)
		return
	}
	base, err := harness.NewSnapshot(*snap.teamsCfg, digests, "refine:assess")
	if err != nil {
		log.Warn("refinement: assessment skipped, snapshot failed",
			"task_id", snap.taskID, "error", err)
		return
	}
	k := rc.MaxCandidates
	if k < 1 {
		k = 3
	}
	var lessons []improvement.Lesson
	if strings.TrimSpace(rc.LessonsDir) != "" {
		if ls, err := improvement.NewStore(rc.LessonsDir).List(ctx, snap.team, 8); err == nil {
			lessons = ls
		} else {
			log.Warn("refinement: assessment lessons unavailable", "task_id", snap.taskID, "error", err)
		}
	}
	general, domain, _, err := buildAssessmentEditor(rc).Propose(ctx, base, snap.team, k, lessons)
	if err != nil {
		log.Warn("refinement: assessment skipped, editor failed",
			"task_id", snap.taskID, "error", err)
		return
	}
	var candidates []improvement.ProposedCandidate
	idx := 0
	for _, es := range general {
		candidates = append(candidates, improvement.ProposedCandidate{Index: idx, Kind: "general", Edits: es.Edits})
		idx++
	}
	for _, es := range domain {
		candidates = append(candidates, improvement.ProposedCandidate{Index: idx, Kind: "domain", Edits: es.Edits})
		idx++
	}
	digest := snap.harnessDigest
	if digest == "" {
		digest = base.ID
	}
	rec, err := improvement.NewStore(rc.LessonsDir).RecordAssessment(ctx, improvement.Assessment{
		Team:          snap.team,
		TaskID:        snap.taskID,
		TaskStatus:    snap.status,
		StepCount:     snap.steps,
		ErrorSteps:    snap.errorSteps,
		HarnessDigest: digest,
		Candidates:    candidates,
		Source:        "refine:assess",
	})
	if err != nil {
		log.Warn("refinement: assessment record failed", "task_id", snap.taskID, "error", err)
		return
	}
	log.Info("refinement: recorded assessment",
		"task_id", snap.taskID, "assessment_id", shortID(rec.ID),
		"status", snap.status, "candidates", len(candidates))
}
