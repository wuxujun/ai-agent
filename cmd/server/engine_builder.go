package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wuxujun/ai-agent/internal/answerpipeline"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/diagnostics"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/testgen"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/uncertainty"
)

type engineBuild struct {
	engine  *orchestrator.Engine
	metrics *metrics.Collector
	runtime *llmcore.Runtime
}

type ollamaProbe func(context.Context, string, string) error

func buildEngine(ctx context.Context, cfg *config.Config, st store.Store, probe ollamaProbe) (engineBuild, error) {
	mode := orchestrator.Mode(cfg.Orchestrator.Mode)
	startupScene := config.LLMSceneTaskPlanner
	if mode == orchestrator.ModeMultiAgent {
		startupScene = config.LLMSceneMultiAgentPlanner
	}
	resolvedLLM := cfg.ResolveLLMScene(startupScene)
	llmProvider := planner.ProviderType(resolvedLLM.Provider)
	providerSpec, providerRegistered := llmprovider.Lookup(string(llmProvider))
	requiresAPIKey := !providerRegistered || providerSpec.RequiresAPIKey
	if resolvedLLM.APIKey == "" && requiresAPIKey {
		if mode == orchestrator.ModeEino || mode == orchestrator.ModeLegacy || mode == orchestrator.ModeMultiAgent {
			return engineBuild{}, fmt.Errorf("%s provider requires an API Key", llmProvider)
		}
		slog.Warn("LLM API key missing", "provider", llmProvider, "mode", mode)
	}

	if llmProvider == planner.ProviderOllama && probe != nil {
		slog.Info("Probing Ollama planner model on startup...", "model", resolvedLLM.Model)
		if err := probe(ctx, resolvedLLM.BaseURL, resolvedLLM.Model); err != nil {
			return engineBuild{}, fmt.Errorf("Ollama planner check failed: %w", err)
		}
		slog.Info("Ollama planner model self-check passed", "model", resolvedLLM.Model)
	}
	embedLLM := cfg.ResolveLLMScene(config.LLMSceneEmbedding)
	if planner.ProviderType(embedLLM.Provider) == planner.ProviderOllama && probe != nil {
		slog.Info("Probing Ollama embedding model on startup...", "model", embedLLM.Model)
		if err := probe(ctx, embedLLM.BaseURL, embedLLM.Model); err != nil {
			return engineBuild{}, fmt.Errorf("Ollama embedding check failed: %w", err)
		}
		slog.Info("Ollama embedding model self-check passed", "model", embedLLM.Model)
	}
	slog.Info("LLM provider configured",
		"configured_provider", cfg.LLM.Provider,
		"provider", llmProvider,
		"scene", startupScene,
		"base_url", resolvedLLM.BaseURL,
		"model", resolvedLLM.Model,
	)

	mc := metrics.NewCollector()
	if postgresStore, ok := st.(*store.PostgresStore); ok {
		postgresStore.SetRetrievalMetrics(mc)
	}
	runtime := llmcore.NewDefaultRuntime(mc)
	plannerClient := planner.NewLLMPlannerForScene(config.LLMSceneTaskPlanner)
	plannerClient.Compressor = planner.NewLLMContextCompressor(config.LLMSceneContextCompressor)
	plannerClient.ArgumentRepairer = planner.NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)
	combinedPlanner := &planner.FallbackPlanner{Primary: plannerClient, Metrics: mc}
	if fallbackScene := cfg.ResolveLLMScene(config.LLMSceneTaskPlanner).FallbackScene; fallbackScene != "" {
		secondaryPlanner := planner.NewLLMPlannerForScene(fallbackScene)
		secondaryPlanner.Compressor = planner.NewLLMContextCompressor(config.LLMSceneContextCompressor)
		secondaryPlanner.ArgumentRepairer = planner.NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)
		combinedPlanner.Secondary = secondaryPlanner
	}

	eng := &orchestrator.Engine{
		Planner:  combinedPlanner,
		Executor: &executor.DefaultExecutor{},
		Metrics:  mc,
		Mode:     mode,
		Store:    st,
	}
	eng.Finalizer = planner.NewLLMTaskFinalizer(config.LLMSceneTaskFinalizer)
	eng.CitationVerifier = planner.NewLLMCitationVerifier(config.LLMSceneCitationVerifier)
	eng.SafetyGuard = policy.NewLLMSafetyGuard(config.LLMSceneSafetyGuard)
	eng.IntentRouter = planner.NewLLMIntentRouter(config.LLMSceneIntentRouter)
	eng.MemoryConflictResolver = memory.NewLLMMemoryConflictResolver(config.LLMSceneMemoryConflictResolver)
	eng.CodeReviewer = review.NewLLMCodeReviewer(config.LLMSceneCodeReviewer)
	eng.TestGenerator = testgen.NewLLMGenerator(config.LLMSceneTestGenerator)
	eng.FailureDiagnoser = diagnostics.NewLLMDiagnoser(config.LLMSceneFailureDiagnoser)
	eng.PlanCritic = plancritic.NewLLMCritic(config.LLMScenePlanCritic)
	eng.PromptInjectionDetector = promptguard.NewLLMDetector(config.LLMScenePromptInjectionDetector)
	eng.EvidenceRelevanceFilter = evidencefilter.NewLLMFilter(config.LLMSceneEvidenceRelevanceFilter)
	eng.EvidenceConflictResolver = evidenceconflict.NewLLMResolver(config.LLMSceneEvidenceConflictResolver)
	eng.SourceCredibilityScorer = sourcecredibility.NewLLMScorer(config.LLMSceneSourceCredibilityScorer)
	eng.FactFreshnessChecker = factfreshness.NewLLMChecker(config.LLMSceneFactFreshnessChecker)
	eng.NumericConsistencyChecker = numericconsistency.NewLLMChecker(config.LLMSceneNumericConsistencyChecker)
	eng.AnswerUncertaintyCalibrator = uncertainty.NewLLMCalibrator(config.LLMSceneAnswerUncertaintyCalibrator)
	eng.AnswerPipeline = &answerpipeline.DefaultPipeline{
		CitationVerifier:      eng.CitationVerifier,
		FreshnessChecker:      eng.FactFreshnessChecker,
		NumericChecker:        eng.NumericConsistencyChecker,
		UncertaintyCalibrator: eng.AnswerUncertaintyCalibrator,
		SafetyGuard:           eng.SafetyGuard,
		SceneEnabled:          eng.LLMSceneEnabled,
		ObserveTokens: func(usage types.TokenUsage, operation string) {
			mc.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, operation)
		},
		ObserveReport: mc.ObserveAnswerPipeline,
	}
	eng.Coordinator = multiagent.NewCoordinator(mc)
	eng.Coordinator.Verifier = &multiagent.VerifierAgent{}
	eng.Coordinator.SuspendForApproval = eng.SuspendForApproval
	eng.Coordinator.ResolveMemoryConflicts = eng.ResolveMemoryConflicts
	eng.Coordinator.PersistTask = st.SaveFullTask
	if mode == orchestrator.ModeMultiAgent {
		slog.Info("multi-agent mode enabled", "coordinator_provider", os.Getenv("AI_AGENT_LLM_PROVIDER"))
	}
	return engineBuild{engine: eng, metrics: mc, runtime: runtime}, nil
}
