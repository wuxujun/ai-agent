package orchestrator

import (
	"context"
	"sync"

	"github.com/wuxujun/ai-agent/internal/answerpipeline"
	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/diagnostics"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/factfreshness"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/numericconsistency"
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
	"go.opentelemetry.io/otel"
	"google.golang.org/adk/model"
)

type Engine struct {
	BrainPinner                 brain.SnapshotPinner
	AnswerPipeline              answerpipeline.Pipeline
	Planner                     planner.Planner
	Finalizer                   planner.TaskFinalizer
	CitationVerifier            planner.CitationVerifier
	SafetyGuard                 policy.SafetyGuard
	IntentRouter                planner.IntentRouter
	MemoryConflictResolver      memory.ConflictResolver
	CodeReviewer                review.CodeReviewer
	CollectCodeChanges          func(context.Context, string) (review.ChangeSet, error)
	TestGenerator               testgen.Generator
	FailureDiagnoser            diagnostics.Diagnoser
	PlanCritic                  plancritic.Critic
	PromptInjectionDetector     promptguard.Detector
	EvidenceRelevanceFilter     evidencefilter.Filter
	EvidenceConflictResolver    evidenceconflict.Resolver
	SourceCredibilityScorer     sourcecredibility.Scorer
	FactFreshnessChecker        factfreshness.Checker
	NumericConsistencyChecker   numericconsistency.Checker
	AnswerUncertaintyCalibrator uncertainty.Calibrator
	Executor                    executor.Executor
	Metrics                     *metrics.Collector
	Mode                        Mode
	AdkModel                    model.LLM
	LLMSceneEnabled             func(string) bool
	// Coordinator is required when Mode == ModeMultiAgent.
	Coordinator *multiagent.Coordinator
	// Store handles database persistence and long-term memory.
	Store store.Store

	// Approvals is the in-memory approval state store for this engine instance.
	// When nil, the engine falls back to defaultApprovals (the process-wide
	// singleton). Tests that require isolation should set this to a fresh
	// NewApprovalStore() before use.
	//
	// P1-1: replacing the implicit global-state coupling with an explicit field
	// so SuspendForApproval no longer depends on package-level variables.
	Approvals *ApprovalStore
	// ApprovalCodec encrypts sensitive action/result payloads before durable
	// persistence. A nil codec preserves legacy in-memory approval behavior and
	// deliberately disables durable approval writes.
	ApprovalCodec ApprovalPayloadCodec

	// ApprovalBus enables cross-instance approval and cancel signalling via
	// Redis Pub/Sub. When nil the engine operates with in-process channels only
	// (single-instance mode). When set, remote approve/reject signals published
	// by any peer instance are forwarded into the local approval channel by the
	// bus's background loop, so SuspendForApproval's select sees them without
	// any extra logic.
	ApprovalBus *ApprovalBus

	// einoRunner is compiled once and cached for the lifetime of the Engine.
	// A sync.RWMutex guards lazy initialisation; unlike sync.Once, a failed
	// compilation attempt can be retried on the next call.
	//
	// Hot path (runner already compiled): acquired as RLock so concurrent
	// requests do not block each other at all.
	// Cold path (first compile or retry): upgraded to full Lock with a
	// double-check to prevent redundant compilations.
	einoMu     sync.RWMutex
	einoRunner any  // compose.Runnable[*einoStepState, *types.Task] after successful compile
	einoReady  bool // true once einoRunner has been successfully compiled

	EventCallback    func(taskID string, status types.TaskStatus)
	ApprovalCallback func(taskID string, approval *types.ApprovalRequest)
	StepCallback     func(taskID string, status types.TaskStatus, step *types.StepTrace)
	TokenCallback    func(taskID string, token string)

	// adkRunner is compiled once and cached for reuse across all runAdkNext calls.
	adkOnce   sync.Once
	adkRunner any // *runner.Runner
	adkErr    error
}

type ApprovalPayloadCodec interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(payload []byte) ([]byte, error)
}

var tracer = otel.Tracer("ai-agent/orchestrator")

// engineLog is the package-level logger for engine.go; the package-level "log"
// var is declared in eino.go and shared across the package.
var engineLog = logger.Component("orchestrator")
