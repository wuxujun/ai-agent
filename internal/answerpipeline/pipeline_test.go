package answerpipeline

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type citationStub struct{}

func (citationStub) Verify(context.Context, *types.Task, string) (*planner.CitationVerification, types.TokenUsage, error) {
	return &planner.CitationVerification{Supported: true, VerifiedAnswer: "rewritten current answer"}, types.TokenUsage{}, nil
}

type freshnessCapture struct{ answer string }

func (f *freshnessCapture) Check(_ context.Context, _ *types.Task, answer string) (*factfreshness.Result, types.TokenUsage, error) {
	f.answer = answer
	return &factfreshness.Result{TimeSensitive: false, Status: "not_applicable", Summary: "not temporal"}, types.TokenUsage{}, nil
}

type guardCounter struct{ calls int }

func (g *guardCounter) Evaluate(context.Context, policy.SafetyStage, *types.Task, string) (*policy.SafetyDecision, types.TokenUsage, error) {
	g.calls++
	return &policy.SafetyDecision{Allowed: true, SafeText: "safe"}, types.TokenUsage{}, nil
}

func TestCitationRewriteFeedsFreshness(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.AnswerPipeline.Enabled = true
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			config.LLMSceneCitationVerifier: {}, config.LLMSceneFactFreshnessChecker: {},
		}
	}))
	freshness := &freshnessCapture{}
	pipeline := &DefaultPipeline{CitationVerifier: citationStub{}, FreshnessChecker: freshness}
	task := &types.Task{FinalAnswer: "old answer", Trace: []types.StepTrace{{Action: "web_search", Observation: "source", Evidence: []types.Evidence{{Path: "source", Lines: []string{"fact"}}}}}}
	if _, err := pipeline.Process(context.Background(), task, "multiagent"); err != nil {
		t.Fatal(err)
	}
	if freshness.answer != "rewritten current answer" {
		t.Fatalf("freshness saw %q", freshness.answer)
	}
}

func TestInputRefusalReusesInputSafetyDecision(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.AnswerPipeline.Enabled = true }))
	guard := &guardCounter{}
	pipeline := &DefaultPipeline{SafetyGuard: guard, SceneEnabled: func(string) bool { return true }}
	task := &types.Task{FinalAnswer: "request refused", Trace: []types.StepTrace{{Action: "safety_guard_input", Observation: "allowed=false categories=malware"}}}
	report, err := pipeline.Process(context.Background(), task, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if guard.calls != 0 || len(report.Stages) != 5 || report.Stages[4].Reason != "covered_by_input_guard" {
		t.Fatalf("calls=%d report=%+v", guard.calls, report)
	}
}
