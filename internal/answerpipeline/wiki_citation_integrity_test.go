package answerpipeline

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestWikiCitationIntegrity(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	const fetched = "wiki://local/concepts/pbl-new-york"
	tests := []struct {
		name         string
		answer       string
		team         string
		wantStatus   string
		wantFindings []string
	}{
		{name: "fetched citation", answer: "Supported fact [1].\n\n[1] [Course](" + fetched + ")", team: "wiki", wantStatus: "passed"},
		{name: "unfetched citation", answer: "[Other](wiki://local/entities/other)", team: "wiki", wantStatus: "warned", wantFindings: []string{"unfetched_wiki_citation"}},
		{name: "cross space citation", answer: "[Other](wiki://tenant-b/concepts/pbl-new-york)", team: "wiki", wantStatus: "warned", wantFindings: []string{"unfetched_wiki_citation"}},
		{name: "invalid citation", answer: "wiki://local/concepts/%2e%2e", team: "wiki", wantStatus: "warned", wantFindings: []string{"invalid_wiki_uri"}},
		{name: "missing citation", answer: "Supported fact without a reference.", team: "wiki", wantStatus: "warned", wantFindings: []string{"missing_wiki_citation"}},
		{name: "suggestion targets are exempt", answer: "Review wiki://local/entities/other", team: "wiki_suggest", wantStatus: "not_applicable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := &types.Task{Team: test.team, FinalAnswer: test.answer, Status: types.StatusCompleted, Trace: []types.StepTrace{{
				Action: "wiki_fetch", Evidence: []types.Evidence{{Path: fetched, Lines: []string{"evidence"}}},
			}}}
			report, err := (&DefaultPipeline{}).Process(context.Background(), task, "multiagent")
			if err != nil {
				t.Fatal(err)
			}
			stage := answerAuditStage(report, wikiCitationIntegrityStage)
			if stage.Status != test.wantStatus {
				t.Fatalf("stage = %+v, want status %q", stage, test.wantStatus)
			}
			if len(stage.Findings) != len(test.wantFindings) {
				t.Fatalf("findings = %+v, want kinds %v", stage.Findings, test.wantFindings)
			}
			for i, kind := range test.wantFindings {
				if stage.Findings[i].Kind != kind {
					t.Fatalf("finding[%d] = %+v, want kind %q", i, stage.Findings[i], kind)
				}
			}
		})
	}
}

func TestWikiCitationIntegrityDoesNotTreatSearchResultsAsFetchedEvidence(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	task := &types.Task{Team: "wiki", FinalAnswer: "[Result](wiki://local/concepts/search-only)", Status: types.StatusCompleted, Trace: []types.StepTrace{{
		Action: "wiki_search", Observation: `{"results":[{"source":"wiki://local/concepts/search-only"}]}`,
	}}}
	report, err := (&DefaultPipeline{}).Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	stage := answerAuditStage(report, wikiCitationIntegrityStage)
	if stage.Status != "warned" || len(stage.Findings) != 1 || stage.Findings[0].Kind != "unfetched_wiki_citation" {
		t.Fatalf("stage = %+v", stage)
	}
}

func TestWikiCitationIntegrityEnforcementModes(t *testing.T) {
	for _, test := range []struct {
		mode            string
		wantStage       string
		wantTask        types.TaskStatus
		wantPublishable bool
	}{
		{mode: "observe", wantStage: "warned", wantTask: types.StatusCompleted, wantPublishable: true},
		{mode: "advisory", wantStage: "failed", wantTask: types.StatusPartial, wantPublishable: true},
		{mode: "strict", wantStage: "failed", wantTask: types.StatusPartial, wantPublishable: false},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
				cfg.AnswerPipeline.Enabled = true
				cfg.AnswerPipeline.Enforcement = test.mode
				cfg.AnswerPipeline.RequiredStages = []string{wikiCitationIntegrityStage}
				cfg.AnswerPipeline.OnRequiredStageFailure = "partial"
				cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
			}))
			task := wikiCitationTask("answer without a citation")
			report, err := (&DefaultPipeline{}).Process(context.Background(), task, "multiagent")
			if err != nil {
				t.Fatal(err)
			}
			if stage := answerAuditStage(report, wikiCitationIntegrityStage); stage.Status != test.wantStage {
				t.Fatalf("stage = %+v, want %q", stage, test.wantStage)
			}
			if task.Status != test.wantTask || report.Publishable != test.wantPublishable {
				t.Fatalf("task status=%q publishable=%t, want status=%q publishable=%t", task.Status, report.Publishable, test.wantTask, test.wantPublishable)
			}
		})
	}
}

func TestWikiCitationIntegrityReauditDoesNotReuseObserveWarningInStrictMode(t *testing.T) {
	restoreObserve := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "observe"
		cfg.AnswerPipeline.RequiredStages = []string{wikiCitationIntegrityStage}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	})
	task := wikiCitationTask("answer without a citation")
	pipeline := &DefaultPipeline{}
	first, err := pipeline.Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	if answerAuditStage(first, wikiCitationIntegrityStage).Status != "warned" {
		t.Fatalf("observe report = %+v", first)
	}
	restoreObserve()
	restoreStrict := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "strict"
		cfg.AnswerPipeline.RequiredStages = []string{wikiCitationIntegrityStage}
		cfg.AnswerPipeline.OnRequiredStageFailure = "partial"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	})
	defer restoreStrict()

	task.Status = types.StatusCompleted
	second, err := pipeline.Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	if stage := answerAuditStage(second, wikiCitationIntegrityStage); stage.Status != "failed" || stage.Fingerprint == answerAuditStage(first, wikiCitationIntegrityStage).Fingerprint {
		t.Fatalf("strict stage = %+v, observe stage = %+v", stage, answerAuditStage(first, wikiCitationIntegrityStage))
	}
	if second.Publishable || task.Status != types.StatusPartial {
		t.Fatalf("task=%+v report=%+v", task, second)
	}
}

func TestWikiCitationIntegrityTenantPolicyOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "observe"
		cfg.AnswerPipeline.RequiredStages = nil
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {
				AnswerPipelineEnforcement:    "advisory",
				AnswerPipelineRequiredStages: []string{wikiCitationIntegrityStage},
			},
		}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	task := wikiCitationTask("answer without a citation")
	task.TenantID = "tenant-a"
	report, err := (&DefaultPipeline{}).Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	if report.Enforcement != "advisory" || answerAuditStage(report, wikiCitationIntegrityStage).Status != "failed" || task.Status != types.StatusPartial || !report.Publishable {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
}

func TestWikiCitationIntegrityStrictModePublishesValidFetchedCitation(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.AnswerPipeline.Enforcement = "strict"
		cfg.AnswerPipeline.RequiredStages = []string{wikiCitationIntegrityStage}
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{}
	}))
	task := wikiCitationTask("[Course](wiki://local/concepts/pbl-new-york)")
	report, err := (&DefaultPipeline{}).Process(context.Background(), task, "multiagent")
	if err != nil {
		t.Fatal(err)
	}
	if answerAuditStage(report, wikiCitationIntegrityStage).Status != "passed" || task.Status != types.StatusCompleted || !report.Publishable {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
}

func wikiCitationTask(answer string) *types.Task {
	return &types.Task{Team: "wiki", FinalAnswer: answer, Status: types.StatusCompleted, Trace: []types.StepTrace{{
		Action: "wiki_fetch", Evidence: []types.Evidence{{Path: "wiki://local/concepts/pbl-new-york", Lines: []string{"evidence"}}},
	}}}
}

func answerAuditStage(report *types.AnswerAuditReport, name string) types.AnswerAuditStage {
	for _, stage := range report.Stages {
		if stage.Name == name {
			return stage
		}
	}
	return types.AnswerAuditStage{}
}
