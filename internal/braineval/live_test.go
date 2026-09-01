package braineval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestLiveRunner_UsesThreeMatchedRepetitionsAndMedian(t *testing.T) {
	answerer := &fakeLiveAnswerer{answers: []AnswerResult{
		{Answer: "wrong", Usage: types.TokenUsage{TotalTokens: 10}, CostUSD: .10, Latency: 10 * time.Millisecond},
		{Answer: "Mei Lin", Usage: types.TokenUsage{TotalTokens: 20}, CostUSD: .20, Latency: 20 * time.Millisecond},
		{Answer: "Mei Lin", Usage: types.TokenUsage{TotalTokens: 30}, CostUSD: .30, Latency: 30 * time.Millisecond},
	}}
	judge := &fakeLiveJudge{results: []JudgeResult{
		{Score: 0, Reason: "wrong", Usage: types.TokenUsage{TotalTokens: 1}, CostUSD: .01},
		{Score: 1, Reason: "correct", Usage: types.TokenUsage{TotalTokens: 2}, CostUSD: .02},
		{Score: 1, Reason: "correct", Usage: types.TokenUsage{TotalTokens: 3}, CostUSD: .03},
	}}
	r := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 3, MaxTotalTokens: 1000, MaxTotalCostUSD: 10})
	c := Case{Name: "decision_release_owner", Scope: scopeAtlas, Query: "当前发布负责人是谁？", ExpectedClaims: []string{"Mei Lin"}}
	offline := VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{{Path: "wiki://atlas-north/projects/decisions", Lines: []string{"Mei Lin"}}}}

	got, err := r.RunVariant(context.Background(), c, offline)
	if err != nil {
		t.Fatal(err)
	}
	if answerer.Calls() != 3 || judge.Calls() != 3 {
		t.Fatalf("writer calls=%d judge calls=%d, want 3 each", answerer.Calls(), judge.Calls())
	}
	if got.MedianUsage != (types.TokenUsage{TotalTokens: 22}) {
		t.Fatalf("median usage = %#v, want writer and judge median totals", got.MedianUsage)
	}
	if got.CaseResult.Usage != got.MedianUsage || got.CaseResult.CostUSD != .22 {
		t.Fatalf("median resources not selected: %#v", got.CaseResult)
	}
	if got.CaseResult.AnswerAccuracy != 1 || got.CaseResult.JudgeScore != 1 || got.CaseResult.Latency != 20*time.Millisecond {
		t.Fatalf("median metrics not selected: %#v", got.CaseResult)
	}
	if !got.CaseResult.Unstable || !slices.Contains(got.UnstableCases, c.Name) {
		t.Fatalf("answer instability not reported: %#v", got)
	}
}

func TestLiveRunner_RunPairUsesOneWriterContractForBothArms(t *testing.T) {
	answerer := &fakeLiveAnswerer{answers: []AnswerResult{
		{Answer: "baseline"}, {Answer: "baseline"}, {Answer: "baseline"},
		{Answer: "Mei Lin"}, {Answer: "Mei Lin"}, {Answer: "Mei Lin"},
	}}
	r := NewLiveRunner(answerer, &fakeLiveJudge{}, LiveOptions{MaxTotalTokens: 1000, MaxTotalCostUSD: 1})
	c := Case{Name: "matched", Scope: scopeAtlas, Query: "owner", ExpectedClaims: []string{"Mei Lin"}}
	pair := PairResult{
		Case:       c,
		Baseline:   VariantOutput{Variant: VariantBaseline, Evidence: []types.Evidence{{Path: "memory://owner", Lines: []string{"Ari Chen"}}}},
		Candidate:  VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{{Path: "wiki://atlas/projects/owner", Lines: []string{"Mei Lin"}}}},
		Comparable: true,
	}

	got, err := r.RunPair(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Comparable || answerer.Calls() != 6 {
		t.Fatalf("pair=%#v writer calls=%d", got, answerer.Calls())
	}
	calls := answerer.RecordedCalls()
	for i, call := range calls {
		if call.caseDef.Name != c.Name || call.caseDef.Query != c.Query || len(call.output.Evidence) != 1 {
			t.Fatalf("call %d changed the shared contract: %#v", i, call)
		}
		wantVariant := VariantBaseline
		wantPath := "memory://owner"
		if i >= 3 {
			wantVariant = VariantBrain
			wantPath = "wiki://atlas/projects/owner"
		}
		if call.output.Variant != wantVariant || call.output.Evidence[0].Path != wantPath {
			t.Fatalf("call %d = %#v, want variant=%s evidence=%s", i, call, wantVariant, wantPath)
		}
	}
}

func TestNewLiveLLMRunner_SnapshotsScenesForAllMatchedCalls(t *testing.T) {
	noFallback := ""
	writerRetries := 1
	judgeRetries := 0
	price := 2.0
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes[config.LLMSceneTaskFinalizer] = config.LLMEndpointConfig{
			Provider:                "openai",
			APIKey:                  "writer-secret",
			Model:                   "writer-frozen",
			FallbackScene:           &noFallback,
			MaxRetries:              &writerRetries,
			InputCostPerMillionUSD:  &price,
			OutputCostPerMillionUSD: &price,
		}
		cfg.LLM.Scenes[config.LLMSceneAnswerVerifier] = config.LLMEndpointConfig{
			Provider:                "openai",
			APIKey:                  "judge-secret",
			Model:                   "judge-frozen",
			FallbackScene:           &noFallback,
			MaxRetries:              &judgeRetries,
			InputCostPerMillionUSD:  &price,
			OutputCostPerMillionUSD: &price,
		}
	})
	runner, err := NewLiveLLMRunner(LiveOptions{Repetitions: 2, MaxTotalTokens: 100, MaxTotalCostUSD: 1})
	restore()
	if err != nil {
		t.Fatal(err)
	}

	caller := &sceneSnapshotCaller{}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	pair := PairResult{
		Case:       Case{Name: "snapshot", Query: "owner", ExpectedClaims: []string{"Mei Lin"}},
		Baseline:   VariantOutput{Variant: VariantBaseline},
		Candidate:  VariantOutput{Variant: VariantBrain},
		Comparable: true,
	}
	got, err := runner.RunPair(ctx, pair)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Comparable {
		t.Fatalf("pair = %#v", got)
	}

	configs := caller.Configs()
	if len(configs) != 8 {
		t.Fatalf("LLM calls = %d, want 8", len(configs))
	}
	for i, cfg := range configs {
		wantModel := "writer-frozen"
		wantRetries := writerRetries
		if i%2 == 1 {
			wantModel = "judge-frozen"
			wantRetries = judgeRetries
		}
		if cfg.Model != wantModel || cfg.MaxRetries != wantRetries || cfg.FallbackScene != "" {
			t.Fatalf("call %d config = %+v, want model=%q retries=%d and no fallback", i, cfg, wantModel, wantRetries)
		}
	}
}

func TestLiveRunner_DoesNotRetryCanceledWriterOrFailedJudge(t *testing.T) {
	t.Run("writer context cancellation", func(t *testing.T) {
		answerer := &fakeLiveAnswerer{answers: []AnswerResult{{Answer: "partial", Usage: types.TokenUsage{TotalTokens: 7}}}, errs: []error{context.Canceled}}
		r := NewLiveRunner(answerer, &fakeLiveJudge{}, LiveOptions{Repetitions: 3, MaxTotalTokens: 100, MaxTotalCostUSD: 1})

		got, err := r.RunVariant(context.Background(), Case{Name: "cancelled", Query: "owner"}, VariantOutput{Variant: VariantBaseline})
		if !errors.Is(err, context.Canceled) || answerer.Calls() != 1 {
			t.Fatalf("error=%v calls=%d", err, answerer.Calls())
		}
		if !got.CaseResult.AnswerAttempted || got.CaseResult.Answer != "partial" || got.CaseResult.Usage.TotalTokens != 7 {
			t.Fatalf("partial writer result was not preserved: %#v", got.CaseResult)
		}
	})

	t.Run("judge failure", func(t *testing.T) {
		answerer := &fakeLiveAnswerer{answers: []AnswerResult{{Answer: "Mei Lin", Usage: types.TokenUsage{TotalTokens: 7}, CostUSD: .07, Latency: time.Second}}}
		judge := &fakeLiveJudge{results: []JudgeResult{{Usage: types.TokenUsage{TotalTokens: 3}, CostUSD: .03}}, errs: []error{errors.New("judge unavailable")}}
		r := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 3, MaxTotalTokens: 100, MaxTotalCostUSD: 1})

		got, err := r.RunVariant(context.Background(), Case{Name: "judge", Query: "owner", ExpectedClaims: []string{"Mei Lin"}}, VariantOutput{Variant: VariantBrain})
		if !errors.Is(err, ErrLiveJudgeFailed) || judge.Calls() != 1 || answerer.Calls() != 1 {
			t.Fatalf("error=%v writer calls=%d judge calls=%d", err, answerer.Calls(), judge.Calls())
		}
		result := got.CaseResult
		if result.Answer != "Mei Lin" || result.Usage.TotalTokens != 10 || result.CostUSD != .10 || result.Latency != time.Second || result.JudgeError == "" {
			t.Fatalf("judge failure lost writer/resources: %#v", result)
		}
		if summary := Summarize([]CaseResult{result}, VariantBrain); summary.JudgeFailures != 1 || summary.Errors != 1 {
			t.Fatalf("judge failure did not fail live summary: %#v", summary)
		}
	})
}

func TestLiveRunner_CancellationAfterWriterPreservesAnswerAndSkipsJudge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	answerer := &cancelingLiveAnswerer{
		cancel: cancel,
		result: AnswerResult{
			Answer:  "Mei Lin",
			Usage:   types.TokenUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
			CostUSD: .07,
			Latency: time.Second,
		},
	}
	judge := &fakeLiveJudge{}
	runner := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 1, MaxTotalTokens: 100, MaxTotalCostUSD: 1})

	got, err := runner.RunVariant(ctx, Case{Name: "cancel-after-writer", Query: "owner", ExpectedClaims: []string{"Mei Lin"}}, VariantOutput{Variant: VariantBrain})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if judge.Calls() != 0 {
		t.Fatalf("judge calls = %d, want zero", judge.Calls())
	}
	result := got.CaseResult
	if result.Answer != answerer.result.Answer || result.Usage != answerer.result.Usage || result.CostUSD != answerer.result.CostUSD || result.Latency != answerer.result.Latency || !result.AnswerAttempted {
		t.Fatalf("completed writer result was not preserved: %#v", result)
	}
}

func TestBudgetTracker_RejectsReservationBeforeMutatingTotals(t *testing.T) {
	b := NewBudgetTracker(10, 1)
	if err := b.Reserve(types.TokenUsage{TotalTokens: 6}, .60); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(types.TokenUsage{TotalTokens: 5}, .20); !errors.Is(err, ErrLiveBudgetExceeded) {
		t.Fatalf("token overrun error = %v", err)
	}
	if err := b.Reserve(types.TokenUsage{TotalTokens: 1}, .50); !errors.Is(err, ErrLiveBudgetExceeded) {
		t.Fatalf("cost overrun error = %v", err)
	}
	tokens, cost := b.Used()
	if tokens != 6 || cost != .60 {
		t.Fatalf("rejected reservations mutated totals: tokens=%d cost=%g", tokens, cost)
	}
}

func TestBudgetTracker_IsAtomicUnderConcurrency(t *testing.T) {
	const limit = 100
	b := NewBudgetTracker(limit, float64(limit))
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for range 400 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if b.Reserve(types.TokenUsage{TotalTokens: 1}, 1) == nil {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	tokens, cost := b.Used()
	if accepted.Load() != limit || tokens != limit || cost != limit {
		t.Fatalf("accepted=%d tokens=%d cost=%g", accepted.Load(), tokens, cost)
	}
}

func TestLiveRunner_BudgetIncludesWriterAndJudgeAndStopsBeforeNextCall(t *testing.T) {
	answerer := &fakeLiveAnswerer{answers: []AnswerResult{{Answer: "one", Usage: types.TokenUsage{TotalTokens: 5}}, {Answer: "two", Usage: types.TokenUsage{TotalTokens: 5}}}}
	judge := &fakeLiveJudge{results: []JudgeResult{{Score: 1, Usage: types.TokenUsage{TotalTokens: 5}}}}
	r := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 3, MaxTotalTokens: 10, MaxTotalCostUSD: 1})

	_, err := r.RunVariant(context.Background(), Case{Name: "budget", Query: "q"}, VariantOutput{Variant: VariantBaseline})
	if !errors.Is(err, ErrLiveBudgetExceeded) {
		t.Fatalf("error = %v, want budget error", err)
	}
	if answerer.Calls() != 1 || judge.Calls() != 1 {
		t.Fatalf("writer calls=%d judge calls=%d, call began after exact cap", answerer.Calls(), judge.Calls())
	}
	tokens, cost := r.Budget().Used()
	if tokens != 10 || cost != 0 {
		t.Fatalf("tracked tokens=%d cost=%g", tokens, cost)
	}
}

func TestLiveRunner_ConcurrentCallsAtomicallyAdmitAndAccountForBudget(t *testing.T) {
	answerer := &concurrentBudgetAnswerer{}
	judge := &fakeLiveJudge{}
	runner := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 1, MaxTotalTokens: 10, MaxTotalCostUSD: 1})
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _ = runner.RunVariant(context.Background(), Case{Name: "concurrent-budget", Query: "q"}, VariantOutput{Variant: VariantBaseline})
		}()
	}
	close(start)
	workers.Wait()

	if calls := answerer.calls.Load(); calls != 1 {
		t.Fatalf("writer calls = %d, want one admitted call", calls)
	}
	if maxActive := answerer.maxActive.Load(); maxActive != 1 {
		t.Fatalf("maximum concurrent writer calls = %d, want 1", maxActive)
	}
	if judge.Calls() != 0 {
		t.Fatalf("judge calls = %d, want zero after writer exhausted budget", judge.Calls())
	}
	if tokens, _ := runner.Budget().Used(); tokens != 10 {
		t.Fatalf("used tokens = %d, want 10", tokens)
	}
}

func TestLiveRunner_RunPairBudgetFailurePreservesBothArmResults(t *testing.T) {
	answerer := &fakeLiveAnswerer{answers: []AnswerResult{
		{Answer: "baseline", Usage: types.TokenUsage{TotalTokens: 2}},
		{Answer: "candidate", Usage: types.TokenUsage{TotalTokens: 2}},
	}}
	judge := &fakeLiveJudge{results: []JudgeResult{
		{Score: 1, Usage: types.TokenUsage{TotalTokens: 2}},
		{Score: 1, Usage: types.TokenUsage{TotalTokens: 2}},
	}}
	r := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 1, MaxTotalTokens: 6, MaxTotalCostUSD: 1})
	pair := PairResult{
		Case:       Case{Name: "budget-pair", Query: "q"},
		Baseline:   VariantOutput{Variant: VariantBaseline},
		Candidate:  VariantOutput{Variant: VariantBrain},
		Comparable: true,
	}

	got, err := r.RunPair(context.Background(), pair)
	if !errors.Is(err, ErrLiveBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
	if got.Comparable || got.Baseline.CaseResult.Comparable || got.Candidate.CaseResult.Comparable {
		t.Fatalf("budget-failed pair remained comparable: %#v", got)
	}
	if got.Baseline.CaseName != pair.Case.Name || got.Candidate.CaseName != pair.Case.Name || got.Baseline.Variant != VariantBaseline || got.Candidate.Variant != VariantBrain {
		t.Fatalf("partial pair lost result identity: %#v", got)
	}
}

func TestLiveRunner_RunPairPropagatesInfrastructureErrorsButKeepsJudgeFailuresInGate(t *testing.T) {
	t.Run("writer infrastructure error", func(t *testing.T) {
		writerErr := errors.New("writer transport unavailable")
		answerer := &fakeLiveAnswerer{
			answers: []AnswerResult{{Answer: "partial", Usage: types.TokenUsage{TotalTokens: 2}}, {Answer: "candidate"}},
			errs:    []error{writerErr},
		}
		runner := NewLiveRunner(answerer, &fakeLiveJudge{}, LiveOptions{Repetitions: 1, MaxTotalTokens: 100, MaxTotalCostUSD: 1})
		pair := PairResult{
			Case:       Case{Name: "writer-infra", Query: "q"},
			Baseline:   VariantOutput{Variant: VariantBaseline},
			Candidate:  VariantOutput{Variant: VariantBrain},
			Comparable: true,
		}

		got, err := runner.RunPair(context.Background(), pair)
		if !errors.Is(err, writerErr) {
			t.Fatalf("error = %v, want writer infrastructure error", err)
		}
		if got.Comparable || got.Baseline.CaseResult.Comparable || got.Candidate.CaseResult.Comparable {
			t.Fatalf("failed pair remained comparable: %#v", got)
		}
		if answerer.Calls() != 1 {
			t.Fatalf("writer calls = %d, candidate ran after baseline infrastructure failure", answerer.Calls())
		}
	})

	t.Run("judge failure is gate-only", func(t *testing.T) {
		judgeErr := errors.New("judge unavailable")
		answerer := &fakeLiveAnswerer{answers: []AnswerResult{{Answer: "baseline"}, {Answer: "candidate"}}}
		judge := &fakeLiveJudge{
			results: []JudgeResult{{Usage: types.TokenUsage{TotalTokens: 1}}, {Score: 1, Reason: "ok", Usage: types.TokenUsage{TotalTokens: 1}}},
			errs:    []error{judgeErr},
		}
		runner := NewLiveRunner(answerer, judge, LiveOptions{Repetitions: 1, MaxTotalTokens: 100, MaxTotalCostUSD: 1})
		pair := PairResult{
			Case:       Case{Name: "judge-gate", Query: "q"},
			Baseline:   VariantOutput{Variant: VariantBaseline},
			Candidate:  VariantOutput{Variant: VariantBrain},
			Comparable: true,
		}

		got, err := runner.RunPair(context.Background(), pair)
		if err != nil {
			t.Fatalf("gate-only judge error propagated: %v", err)
		}
		if !got.Comparable || got.Baseline.JudgeError == "" || got.Candidate.JudgeError == "" {
			t.Fatalf("judge failure was not retained for the gate: %#v", got)
		}
		if answerer.Calls() != 2 || judge.Calls() != 2 {
			t.Fatalf("matched calls writer=%d judge=%d, want 2 each", answerer.Calls(), judge.Calls())
		}
	})
}

func TestFinalizerAnswerer_UsesOnlyDeterministicEvidenceContract(t *testing.T) {
	finalizer := &recordingFinalizer{answer: "Mei Lin", usage: types.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}
	answerer := FinalizerAnswerer{Finalizer: finalizer, Config: llmcore.Config{Scene: "task_finalizer", InputCostPerMillionUSD: 2, OutputCostPerMillionUSD: 3}}
	c := Case{Name: "owner", Scope: scopeAtlas, Query: "who owns release?"}
	out := VariantOutput{Variant: VariantBrain, Evidence: []types.Evidence{{Path: "wiki://atlas/projects/owner", Query: "owner", Lines: []string{"Mei Lin"}}}}

	got, err := answerer.Answer(context.Background(), c, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "Mei Lin" || got.Usage != finalizer.usage || math.Abs(got.CostUSD-26.0/1_000_000) > 1e-12 || got.Latency < 0 {
		t.Fatalf("answer = %#v", got)
	}
	task := finalizer.task
	if task == nil || task.ID != "brain-eval-owner" || task.TenantID != scopeAtlas.TenantID || task.Goal != c.Query || len(task.Trace) != 1 {
		t.Fatalf("task = %#v", task)
	}
	trace := task.Trace[0]
	evidenceEqual := slices.EqualFunc(trace.Evidence, out.Evidence, func(left, right types.Evidence) bool {
		return left.Path == right.Path && left.Query == right.Query && slices.Equal(left.Lines, right.Lines)
	})
	if trace.Step != 1 || trace.Goal != c.Query || trace.Action != "brain_eval_evidence" || trace.Query != c.Query || !evidenceEqual {
		t.Fatalf("trace = %#v", trace)
	}
}

func TestLLMJudge_UsesAnswerVerifierAndStrictSchema(t *testing.T) {
	caller := &recordingStructuredCaller{response: `{"score":0.75,"reason":"mostly correct"}`, usage: types.TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))
	c := Case{Name: "owner", Query: "who owns release?", ExpectedClaims: []string{"Mei Lin"}, ForbiddenClaims: []string{"Ari Chen"}}

	got, err := (LLMJudge{Config: llmcore.Config{Scene: config.LLMSceneAnswerVerifier}}).Judge(ctx, c, "Mei Lin owns release")
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != .75 || got.Reason != "mostly correct" || got.Usage != caller.usage {
		t.Fatalf("judge result = %#v", got)
	}
	if caller.cfg.Scene != "answer_verifier" {
		t.Fatalf("scene = %q", caller.cfg.Scene)
	}
	properties, ok := caller.schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", caller.schema["properties"])
	}
	score, _ := properties["score"].(map[string]any)
	reason, _ := properties["reason"].(map[string]any)
	if caller.schema["type"] != "object" || caller.schema["additionalProperties"] != false || score["minimum"] != 0 || score["maximum"] != 1 || reason["maxLength"] != 1000 {
		t.Fatalf("schema = %#v", caller.schema)
	}
	if !strings.Contains(caller.userPrompt, c.Query) || !strings.Contains(caller.userPrompt, c.ExpectedClaims[0]) || !strings.Contains(caller.userPrompt, "Mei Lin owns release") {
		t.Fatalf("judge prompt omitted case contract: %q", caller.userPrompt)
	}
}

func TestLLMJudge_RejectsOutOfRangeScoreAndPreservesUsage(t *testing.T) {
	caller := &recordingStructuredCaller{response: `{"score":1.1,"reason":"invalid"}`, usage: types.TokenUsage{TotalTokens: 9}}
	ctx := llmcore.WithRuntime(context.Background(), llmcore.NewRuntime(caller, nil))

	got, err := (LLMJudge{Config: llmcore.Config{Scene: config.LLMSceneAnswerVerifier}}).Judge(ctx, Case{Name: "invalid", Query: "q"}, "answer")
	if err == nil || got.Usage.TotalTokens != 9 {
		t.Fatalf("result=%#v error=%v", got, err)
	}
}

func TestValidateLiveSceneConfigs_FailsClosed(t *testing.T) {
	valid := llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer", APIKey: "secret", MaxRetries: 1, InputCostPerMillionUSD: 1, OutputCostPerMillionUSD: 1}
	tests := []struct {
		name   string
		writer llmcore.Config
		judge  llmcore.Config
		want   string
	}{
		{name: "valid", writer: valid, judge: llmcore.Config{Scene: "answer_verifier", Provider: "openai", Model: "judge", APIKey: "secret", InputCostPerMillionUSD: 1, OutputCostPerMillionUSD: 1}},
		{name: "missing provider", writer: llmcore.Config{Scene: "task_finalizer", Model: "writer", APIKey: "secret"}, judge: valid, want: "provider"},
		{name: "missing model", writer: llmcore.Config{Scene: "task_finalizer", Provider: "openai", APIKey: "secret"}, judge: valid, want: "model"},
		{name: "missing credential", writer: llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer"}, judge: valid, want: "credential"},
		{name: "too many retries", writer: valid, judge: llmcore.Config{Scene: "answer_verifier", Provider: "openai", Model: "judge", APIKey: "secret", MaxRetries: 2}, want: "max_retries"},
		{name: "fallback writer", writer: llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer", APIKey: "secret", FallbackScene: "backup"}, judge: valid, want: "fallback"},
		{name: "missing input pricing", writer: llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer", APIKey: "secret", OutputCostPerMillionUSD: 1}, judge: valid, want: "input pricing"},
		{name: "missing output pricing", writer: llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer", APIKey: "secret", InputCostPerMillionUSD: 1}, judge: valid, want: "output pricing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLiveSceneConfigs(tt.writer, tt.judge, 1)
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if err := validateLiveSceneConfigs(
		llmcore.Config{Scene: "task_finalizer", Provider: "openai", Model: "writer", APIKey: "secret"},
		llmcore.Config{Scene: "answer_verifier", Provider: "openai", Model: "judge", APIKey: "secret"},
		0,
	); err != nil {
		t.Fatalf("uncapped validation rejected absent pricing: %v", err)
	}
}

type answerCall struct {
	caseDef Case
	output  VariantOutput
}

type fakeLiveAnswerer struct {
	mu      sync.Mutex
	answers []AnswerResult
	errs    []error
	calls   []answerCall
}

func (f *fakeLiveAnswerer) Answer(_ context.Context, c Case, output VariantOutput) (AnswerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.calls)
	f.calls = append(f.calls, answerCall{caseDef: c, output: output})
	var answer AnswerResult
	if index < len(f.answers) {
		answer = f.answers[index]
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return answer, err
}

func (f *fakeLiveAnswerer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLiveAnswerer) RecordedCalls() []answerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]answerCall(nil), f.calls...)
}

type fakeLiveJudge struct {
	mu      sync.Mutex
	results []JudgeResult
	errs    []error
	calls   int
}

type concurrentBudgetAnswerer struct {
	calls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
}

type cancelingLiveAnswerer struct {
	cancel context.CancelFunc
	result AnswerResult
}

func (a *cancelingLiveAnswerer) Answer(context.Context, Case, VariantOutput) (AnswerResult, error) {
	a.cancel()
	return a.result, nil
}

func (a *concurrentBudgetAnswerer) Answer(context.Context, Case, VariantOutput) (AnswerResult, error) {
	a.calls.Add(1)
	active := a.active.Add(1)
	for {
		maximum := a.maxActive.Load()
		if active <= maximum || a.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	a.active.Add(-1)
	return AnswerResult{Answer: "answer", Usage: types.TokenUsage{TotalTokens: 10}}, nil
}

func (f *fakeLiveJudge) Judge(_ context.Context, c Case, answer string) (JudgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.calls
	f.calls++
	if len(f.results) == 0 {
		score := 0.0
		if len(c.ExpectedClaims) == 0 || strings.Contains(answer, c.ExpectedClaims[0]) {
			score = 1
		}
		return JudgeResult{Score: score, Reason: "deterministic fake"}, nil
	}
	var result JudgeResult
	if index < len(f.results) {
		result = f.results[index]
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeLiveJudge) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingFinalizer struct {
	answer string
	usage  types.TokenUsage
	err    error
	task   *types.Task
}

func (f *recordingFinalizer) Finalize(_ context.Context, task *types.Task) (string, types.TokenUsage, error) {
	f.task = task
	return f.answer, f.usage, f.err
}

type recordingStructuredCaller struct {
	response     string
	usage        types.TokenUsage
	err          error
	cfg          llmcore.Config
	systemPrompt string
	userPrompt   string
	schema       map[string]any
}

type sceneSnapshotCaller struct {
	mu      sync.Mutex
	configs []llmcore.Config
}

func (c *sceneSnapshotCaller) CallJSON(_ context.Context, cfg llmcore.Config, _, _ string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.mu.Lock()
	c.configs = append(c.configs, cfg)
	c.mu.Unlock()
	response := `{"final_answer":"Mei Lin","evidence_summary":"matched","confidence":"high"}`
	if cfg.Scene == config.LLMSceneAnswerVerifier {
		response = `{"score":1,"reason":"matched"}`
	}
	if err := json.Unmarshal([]byte(response), dest); err != nil {
		return types.TokenUsage{}, err
	}
	return types.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, nil
}

func (c *sceneSnapshotCaller) Configs() []llmcore.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llmcore.Config(nil), c.configs...)
}

func (c *recordingStructuredCaller) CallJSON(_ context.Context, cfg llmcore.Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	c.cfg = cfg
	c.systemPrompt = systemPrompt
	c.userPrompt = userPrompt
	c.schema = schema
	if c.err != nil {
		return c.usage, c.err
	}
	if err := json.Unmarshal([]byte(c.response), dest); err != nil {
		return c.usage, fmt.Errorf("decode fake response: %w", err)
	}
	return c.usage, nil
}
