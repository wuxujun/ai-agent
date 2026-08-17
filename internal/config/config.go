package config

import (
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/logger"
)

// Config holds all application configuration loaded from the config file and
// environment variables. Fields are grouped by subsystem.
type Config struct {
	API struct {
		Addr    string                     `mapstructure:"addr"`
		APIKey  string                     `mapstructure:"api_key"`
		Auth    APIAuthConfig              `mapstructure:"auth"`
		Tenants map[string]APITenantConfig `mapstructure:"tenants"`
	} `mapstructure:"api"`

	Store struct {
		Type string `mapstructure:"type"`
		DSN  string `mapstructure:"dsn"`
		// VectorSearch controls how Store.QueryMemories ranks memories.
		// "in_process" keeps the existing JSON load + Go ranking path,
		// "pgvector" enables PostgreSQL vector ranking, and "paradedb" fuses
		// ParadeDB BM25 and pgvector rankings. Non-Postgres backends ignore it.
		VectorSearch string `mapstructure:"vector_search"`
		// PGVectorDimensions optionally enables a pgvector HNSW expression
		// index for embeddings with this exact dimension. 0 keeps pgvector in
		// exact-scan mode, which is still useful for avoiding JSON
		// deserialization in Go but does not provide ANN indexing.
		PGVectorDimensions int `mapstructure:"pgvector_dimensions"`
		// ParadeDBCandidateMultiplier controls how many candidates each BM25
		// and pgvector branch contributes before reciprocal-rank fusion.
		// 0 uses the default (4).
		ParadeDBCandidateMultiplier int `mapstructure:"paradedb_candidate_multiplier"`
		// ParadeDBRRFK is the reciprocal-rank-fusion smoothing constant.
		// Lower values emphasize top ranks; 0 uses the default (60).
		ParadeDBRRFK float64 `mapstructure:"paradedb_rrf_k"`
		// ParadeDBSlowQueryThresholdMS marks individual hybrid retrieval
		// phases as slow. 0 disables slow-phase counting.
		ParadeDBSlowQueryThresholdMS int `mapstructure:"paradedb_slow_query_threshold_ms"`
		// MemoryCandidateLimit caps how many recent memory rows each Store
		// backend loads from disk before in-process cosine/keyword ranking.
		// Without a cap, large memory tables would scan the full table on
		// every RAG prefetch; with the default of 200, only the most recent
		// 200 rows are ever considered. Raise this if recall on older memories
		// matters more than scan latency. 0 falls back to the package default
		// (200).
		MemoryCandidateLimit int `mapstructure:"memory_candidate_limit"`
		// MemoryDecayRate is the rate at which memories decay over time.
		// A decay rate of 0.0 disables time decay. A positive rate (e.g. 0.01 per hour)
		// will reduce the score of older memories exponentially.
		MemoryDecayRate float64 `mapstructure:"memory_decay_rate"`
	} `mapstructure:"store"`

	Orchestrator struct {
		Mode               string `mapstructure:"mode"`
		MaxConcurrentTasks int    `mapstructure:"max_concurrent_tasks"`
		// RunAllTimeoutSeconds caps the wall-clock budget of a single
		// background run-all goroutine (the one launched by POST /api/tasks/:id/run-all).
		// 0 falls back to the package default (600s). Long multiagent tasks may
		// need this raised; strict SLA deployments can lower it.
		RunAllTimeoutSeconds int `mapstructure:"run_all_timeout_seconds"`
	} `mapstructure:"orchestrator"`

	MultiAgent struct {
		// Team selects an entry from teams.yaml. Runtime remains legacy during a
		// percentage rollout; DAGCanaryPercent then assigns new tasks to DAG by
		// stable task-ID bucket.
		Team             string `mapstructure:"team"`
		Runtime          string `mapstructure:"runtime"`
		DAGCanaryPercent int    `mapstructure:"dag_canary_percent"`
	} `mapstructure:"multiagent"`

	Approval struct {
		// TTLSeconds expires unresolved durable approvals lazily on access.
		// Zero disables expiration.
		TTLSeconds int `mapstructure:"ttl_seconds"`
		// RetentionDays controls cleanup of consumed/expired records. Zero disables cleanup.
		RetentionDays int `mapstructure:"retention_days"`
	} `mapstructure:"approval"`

	AnswerPipeline struct {
		Enabled                bool           `mapstructure:"enabled"`
		Enforcement            string         `mapstructure:"enforcement"`
		RequiredStages         []string       `mapstructure:"required_stages"`
		AuditTokenReserve      int            `mapstructure:"audit_token_reserve"`
		StageTimeoutSeconds    int            `mapstructure:"stage_timeout_seconds"`
		ParallelAudits         bool           `mapstructure:"parallel_audits"`
		OnRequiredStageFailure string         `mapstructure:"on_required_stage_failure"`
		StageTokenBudgets      map[string]int `mapstructure:"stage_token_budgets"`
	} `mapstructure:"answer_pipeline"`

	LLM struct {
		Provider                         string  `mapstructure:"provider"`
		APIKey                           string  `mapstructure:"api_key"`
		OpenAIAPIKey                     string  `mapstructure:"openai_api_key"`
		GeminiAPIKey                     string  `mapstructure:"gemini_api_key"`
		GoogleAPIKey                     string  `mapstructure:"google_api_key"`
		Model                            string  `mapstructure:"model"`
		BaseURL                          string  `mapstructure:"base_url"`
		TimeoutSeconds                   int     `mapstructure:"timeout_seconds"`
		ReadinessMode                    string  `mapstructure:"readiness_mode"`
		ReadinessCacheTTLSeconds         int     `mapstructure:"readiness_cache_ttl_seconds"`
		CircuitBreakerFailureThreshold   int     `mapstructure:"circuit_breaker_failure_threshold"`
		CircuitBreakerCooldownSeconds    int     `mapstructure:"circuit_breaker_cooldown_seconds"`
		RetryBudgetPerMinute             int     `mapstructure:"retry_budget_per_minute"`
		MaxCallsPerTask                  int     `mapstructure:"max_calls_per_task"`
		MaxEstimatedCostUSDPerTask       float64 `mapstructure:"max_estimated_cost_usd_per_task"`
		ContextCompressionTraceThreshold int     `mapstructure:"context_compression_trace_threshold"`
		// ContextCompressionTokenThreshold triggers compression when the sum of
		// all TotalTokens across task.Trace exceeds this value, regardless of
		// step count. 0 disables the token-based trigger.
		ContextCompressionTokenThreshold int                          `mapstructure:"context_compression_token_threshold"`
		PlannerTraceMaxItems             int                          `mapstructure:"planner_trace_max_items"`
		PlannerObservationMaxChars       int                          `mapstructure:"planner_observation_max_chars"`
		PlannerEvidenceMaxItems          int                          `mapstructure:"planner_evidence_max_items"`
		PlannerEvidenceLineMaxChars      int                          `mapstructure:"planner_evidence_line_max_chars"`
		PlannerTraceMaxChars             int                          `mapstructure:"planner_trace_max_chars"`
		Gateway                          LLMEndpointConfig            `mapstructure:"gateway"`
		Scenes                           map[string]LLMEndpointConfig `mapstructure:"scenes"`
	} `mapstructure:"llm"`

	Embedding struct {
		Model string `mapstructure:"model"`
	} `mapstructure:"embedding"`

	RAG struct {
		SearchURL              string `mapstructure:"search_url"`
		SearchMethod           string `mapstructure:"search_method"`
		Authorization          string `mapstructure:"authorization"`
		ToolName               string `mapstructure:"tool_name"`
		ContextMode            string `mapstructure:"context_mode"`
		JITSearchMaxCalls      int    `mapstructure:"jit_search_max_calls"`
		JITRetrievalMaxCycles  int    `mapstructure:"jit_retrieval_max_cycles"`
		JITFetchMaxItems       int    `mapstructure:"jit_fetch_max_items"`
		JITRAGFetchMaxBytes    int    `mapstructure:"jit_rag_fetch_max_bytes"`
		JITMemoryFetchMaxBytes int    `mapstructure:"jit_memory_fetch_max_bytes"`
		SessionRecentTaskLimit int    `mapstructure:"session_recent_task_limit"`
		MaxPromptMemories      int    `mapstructure:"max_prompt_memories"`
		MaxMemoryBytes         int    `mapstructure:"max_memory_bytes"`
		MaxMemoryPromptBytes   int    `mapstructure:"max_memory_prompt_bytes"`
		MaxRawFallbackBytes    int    `mapstructure:"max_raw_fallback_bytes"`
	} `mapstructure:"rag"`

	MCP struct {
		Servers []MCPServerConfig `mapstructure:"servers"`
	} `mapstructure:"mcp"`

	// Wiki configures the optional read-only LLM Wiki adapter. Mutating Wiki
	// operations are deliberately not exposed by this integration.
	Wiki struct {
		URL                            string `mapstructure:"url"`
		Directory                      string `mapstructure:"directory"`
		AuthorizationEnv               string `mapstructure:"authorization_env"`
		DefaultSpace                   string `mapstructure:"default_space"`
		TimeoutSeconds                 int    `mapstructure:"timeout_seconds"`
		SearchTopK                     int    `mapstructure:"search_top_k"`
		FetchMaxItems                  int    `mapstructure:"fetch_max_items"`
		FetchMaxBytes                  int    `mapstructure:"fetch_max_bytes"`
		CircuitBreakerFailureThreshold int    `mapstructure:"circuit_breaker_failure_threshold"`
		CircuitBreakerCooldownSeconds  int    `mapstructure:"circuit_breaker_cooldown_seconds"`
		AllowPrivateNetwork            bool   `mapstructure:"allow_private_network"`
		Required                       bool   `mapstructure:"required"`
	} `mapstructure:"wiki"`

	Search struct {
		URL    string `mapstructure:"url"`
		APIKey string `mapstructure:"api_key"`
	} `mapstructure:"search"`

	Log struct {
		Level         string `mapstructure:"level"`
		Console       bool   `mapstructure:"console"`
		FileEnabled   bool   `mapstructure:"file_enabled"`
		AccessEnabled bool   `mapstructure:"access_enabled"`
		Directory     string `mapstructure:"directory"`
		RetentionDays int    `mapstructure:"retention_days"`
	} `mapstructure:"log"`

	Telemetry struct {
		Enabled     bool   `mapstructure:"enabled"`
		Endpoint    string `mapstructure:"endpoint"`
		Environment string `mapstructure:"environment"`
		Exporter    string `mapstructure:"exporter"`
	} `mapstructure:"telemetry"`

	Tool struct {
		TimeoutSeconds int `mapstructure:"timeout_seconds"`
	} `mapstructure:"tool"`

	Skill struct {
		// Root is the directory scanned for <name>/SKILL.md skill packages,
		// relative to the server's working directory unless absolute.
		Root string `mapstructure:"root"`
	} `mapstructure:"skill"`

	Langfuse struct {
		PublicKey               string `mapstructure:"public_key"`
		SecretKey               string `mapstructure:"secret_key"`
		Host                    string `mapstructure:"host"`
		Enabled                 bool   `mapstructure:"enabled"`
		BootstrapMissingPrompts bool   `mapstructure:"bootstrap_missing_prompts"`
		BootstrapFailurePolicy  string `mapstructure:"bootstrap_failure_policy"`
		BootstrapTimeoutSeconds int    `mapstructure:"bootstrap_timeout_seconds"`
	} `mapstructure:"langfuse"`
}

type APITenantConfig struct {
	APIKey                       string   `mapstructure:"api_key"`
	Admin                        bool     `mapstructure:"admin"`
	WorkspaceRoot                string   `mapstructure:"workspace_root"`
	DailyLLMCallBudget           int      `mapstructure:"daily_llm_call_budget"`
	DailyLLMCostBudgetUSD        float64  `mapstructure:"daily_llm_cost_budget_usd"`
	AnswerPipelineEnforcement    string   `mapstructure:"answer_pipeline_enforcement"`
	AnswerPipelineRequiredStages []string `mapstructure:"answer_pipeline_required_stages"`
	// WikiSpace selects the LLM Wiki space visible to this tenant. An empty
	// value falls back to wiki.default_space when that operator-wide sharing is
	// intentional.
	WikiSpace string `mapstructure:"wiki_space"`
}

type APIAuthConfig struct {
	// Mode controls accepted credentials: api_key, jwt, introspection, or hybrid.
	Mode          string                 `mapstructure:"mode"`
	Bearer        APIBearerConfig        `mapstructure:"bearer"`
	JWT           APIJWTConfig           `mapstructure:"jwt"`
	Introspection APIIntrospectionConfig `mapstructure:"introspection"`
	// RequireTenantWorkspaceRoot rejects non-admin task creation unless the
	// authenticated tenant has an explicit workspace_root boundary.
	RequireTenantWorkspaceRoot bool `mapstructure:"require_tenant_workspace_root"`
}

type APIBearerConfig struct {
	// ValidationMode selects jwks or introspection for Bearer tokens in hybrid mode.
	ValidationMode string `mapstructure:"validation_mode"`
}

type APIJWTConfig struct {
	Issuer                    string   `mapstructure:"issuer"`
	Audience                  string   `mapstructure:"audience"`
	JWKSURL                   string   `mapstructure:"jwks_url"`
	TenantClaim               string   `mapstructure:"tenant_claim"`
	AllowedAlgorithms         []string `mapstructure:"allowed_algorithms"`
	RequireKnownTenant        bool     `mapstructure:"require_known_tenant"`
	ClockSkewSeconds          int      `mapstructure:"clock_skew_seconds"`
	JWKSCacheTTLSeconds       int      `mapstructure:"jwks_cache_ttl_seconds"`
	JWKSRequestTimeoutSeconds int      `mapstructure:"jwks_request_timeout_seconds"`
}

type APIIntrospectionConfig struct {
	URL                 string `mapstructure:"url"`
	TenantClaim         string `mapstructure:"tenant_claim"`
	ActiveClaim         string `mapstructure:"active_claim"`
	Issuer              string `mapstructure:"issuer"`
	Audience            string `mapstructure:"audience"`
	RequireExpiration   bool   `mapstructure:"require_expiration"`
	RequireKnownTenant  bool   `mapstructure:"require_known_tenant"`
	AllowPrivateNetwork bool   `mapstructure:"allow_private_network"`
	TimeoutSeconds      int    `mapstructure:"timeout_seconds"`
	CacheTTLSeconds     int    `mapstructure:"cache_ttl_seconds"`
}

// MCPServerConfig describes one operator-configured MCP Streamable HTTP
// server. AuthorizationEnv names an environment variable containing the
// credential so secrets never need to be stored in config.yaml.
type MCPServerConfig struct {
	Name                string `mapstructure:"name"`
	URL                 string `mapstructure:"url"`
	AuthorizationEnv    string `mapstructure:"authorization_env"`
	ToolPrefix          string `mapstructure:"tool_prefix"`
	RiskLevel           string `mapstructure:"risk_level"`
	TimeoutSeconds      int    `mapstructure:"timeout_seconds"`
	MaxTools            int    `mapstructure:"max_tools"`
	Disabled            bool   `mapstructure:"disabled"`
	Required            bool   `mapstructure:"required"`
	AllowPrivateNetwork bool   `mapstructure:"allow_private_network"`
}

// LLMEndpointConfig is a provider/model profile used by a specific LLM scene.
// Empty provider/model fields inherit from llm.gateway and then from the legacy
// llm settings. Pointer policy fields distinguish omission from an explicit
// zero value that clears a gateway default.
type LLMEndpointConfig struct {
	Provider                string         `mapstructure:"provider"`
	APIKey                  string         `mapstructure:"api_key"`
	Model                   string         `mapstructure:"model"`
	BaseURL                 string         `mapstructure:"base_url"`
	TimeoutSeconds          int            `mapstructure:"timeout_seconds"`
	FallbackScene           *string        `mapstructure:"fallback_scene"`
	MaxRetries              *int           `mapstructure:"max_retries"`
	MinRemainingTokens      *int           `mapstructure:"min_remaining_tokens"`
	InputCostPerMillionUSD  *float64       `mapstructure:"input_cost_per_million_usd"`
	OutputCostPerMillionUSD *float64       `mapstructure:"output_cost_per_million_usd"`
	Routes                  []LLMRouteRule `mapstructure:"routes" json:"routes,omitempty"`
}

type LLMRouteRule struct {
	TargetScene        string   `mapstructure:"target_scene" json:"target_scene"`
	MinRemainingTokens *int     `mapstructure:"min_remaining_tokens" json:"min_remaining_tokens,omitempty"`
	MaxRemainingTokens *int     `mapstructure:"max_remaining_tokens" json:"max_remaining_tokens,omitempty"`
	MinStepCount       *int     `mapstructure:"min_step_count" json:"min_step_count,omitempty"`
	MaxStepCount       *int     `mapstructure:"max_step_count" json:"max_step_count,omitempty"`
	Intents            []string `mapstructure:"intents" json:"intents,omitempty"`
	Complexities       []string `mapstructure:"complexities" json:"complexities,omitempty"`
	CostTiers          []string `mapstructure:"cost_tiers" json:"cost_tiers,omitempty"`
	LatencyTiers       []string `mapstructure:"latency_tiers" json:"latency_tiers,omitempty"`
	QualityTiers       []string `mapstructure:"quality_tiers" json:"quality_tiers,omitempty"`
}

type ResolvedLLMConfig struct {
	Provider                string
	APIKey                  string
	Model                   string
	BaseURL                 string
	TimeoutSeconds          int
	FallbackScene           string
	MaxRetries              int
	MinRemainingTokens      int
	InputCostPerMillionUSD  float64
	OutputCostPerMillionUSD float64
}

type LLMRoutingHints struct {
	HasRemainingTokens bool
	RemainingTokens    int
	StepCount          int
	Intent             string
	Complexity         string
	CostTier           string
	LatencyTier        string
	QualityTier        string
}

// ResolveLLMProviderConfig returns provider-specific defaults without carrying
// a model or URL explicitly selected for another provider.
func (c *Config) ResolveLLMProviderConfig(provider string) ResolvedLLMConfig {
	timeout := c.LLM.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	return ResolvedLLMConfig{
		Provider:       provider,
		APIKey:         c.ResolveLLMAPIKey(provider),
		Model:          defaultLLMModel(provider),
		BaseURL:        defaultLLMBaseURL(provider),
		TimeoutSeconds: timeout,
	}
}

const (
	LLMReadinessConfigOnly = "config_only"
	LLMReadinessGateway    = "gateway"
	LLMReadinessInference  = "inference"

	LLMSceneTaskPlanner                 = "task_planner"
	LLMSceneTaskFinalizer               = "task_finalizer"
	LLMSceneCitationVerifier            = "citation_verifier"
	LLMSceneSafetyGuard                 = "safety_guard"
	LLMSceneToolArgumentRepair          = "tool_argument_repair"
	LLMSceneIntentRouter                = "intent_router"
	LLMSceneMemoryConflictResolver      = "memory_conflict_resolver"
	LLMSceneVisionAnalyzer              = "vision_analyzer"
	LLMSceneCodeReviewer                = "code_reviewer"
	LLMSceneTestGenerator               = "test_generator"
	LLMSceneFailureDiagnoser            = "failure_diagnoser"
	LLMScenePlanCritic                  = "plan_critic"
	LLMScenePromptInjectionDetector     = "prompt_injection_detector"
	LLMSceneEvidenceRelevanceFilter     = "evidence_relevance_filter"
	LLMSceneEvidenceConflictResolver    = "evidence_conflict_resolver"
	LLMSceneSourceCredibilityScorer     = "source_credibility_scorer"
	LLMSceneFactFreshnessChecker        = "fact_freshness_checker"
	LLMSceneNumericConsistencyChecker   = "numeric_consistency_checker"
	LLMSceneAnswerUncertaintyCalibrator = "answer_uncertainty_calibrator"
	LLMSceneContextCompressor           = "context_compressor"
	LLMSceneAnswerVerifier              = "answer_verifier"
	LLMSceneMemorySummarizer            = "memory_summarizer"
	LLMSceneRAGQueryRewriter            = "rag_query_rewriter"
	LLMSceneRAGReranker                 = "rag_reranker"
	LLMSceneMultiAgentPlanner           = "multiagent_planner"
	LLMSceneMultiAgentReplanner         = "multiagent_replanner"
	LLMSceneMultiAgentWriter            = "multiagent_writer"
	LLMSceneEmbedding                   = "embedding"
	LLMSceneADK                         = "adk"
)

func (c *Config) ResolveLLMReadinessMode() string {
	mode := strings.TrimSpace(strings.ToLower(c.LLM.ReadinessMode))
	if mode == "" {
		return LLMReadinessGateway
	}
	return mode
}

func (c *Config) ResolveLLMReadinessCacheTTLSeconds() int {
	if c.LLM.ReadinessCacheTTLSeconds <= 0 {
		return 10
	}
	return c.LLM.ReadinessCacheTTLSeconds
}

// mu guards globalConfig for concurrent reads/writes.
// Hot path: RLock for Get(); Cold path: Lock for Reload().
var (
	mu             sync.RWMutex
	globalConfig   *Config
	configRevision uint64
)

// setupViper registers file paths, env-var bindings, and default values on the
// package-level viper instance. Idempotent and safe to call multiple times.
func setupViper() {
	if os.Getenv("TEST_NO_CONFIG") == "true" {
		viper.SetConfigName("non_existent_config_for_testing")
	} else {
		viper.SetConfigName("config")
	}
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("../../")

	// Set Environment Variable Prefix and replace . with _
	viper.SetEnvPrefix("AI_AGENT")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Default values
	viper.SetDefault("api.addr", "127.0.0.1:8080")
	viper.SetDefault("api.api_key", "")
	viper.SetDefault("api.auth.mode", "api_key")
	viper.SetDefault("api.auth.require_tenant_workspace_root", false)
	viper.SetDefault("api.auth.bearer.validation_mode", "jwks")
	viper.SetDefault("api.auth.jwt.tenant_claim", "code")
	viper.SetDefault("api.auth.jwt.allowed_algorithms", []string{"RS256"})
	viper.SetDefault("api.auth.jwt.require_known_tenant", true)
	viper.SetDefault("api.auth.jwt.clock_skew_seconds", 30)
	viper.SetDefault("api.auth.jwt.jwks_cache_ttl_seconds", 300)
	viper.SetDefault("api.auth.jwt.jwks_request_timeout_seconds", 5)
	viper.SetDefault("api.auth.introspection.tenant_claim", "code")
	viper.SetDefault("api.auth.introspection.active_claim", "active")
	viper.SetDefault("api.auth.introspection.require_expiration", false)
	viper.SetDefault("api.auth.introspection.require_known_tenant", true)
	viper.SetDefault("api.auth.introspection.allow_private_network", false)
	viper.SetDefault("api.auth.introspection.timeout_seconds", 3)
	viper.SetDefault("api.auth.introspection.cache_ttl_seconds", 10)
	viper.SetDefault("store.type", "sqlite")
	viper.SetDefault("store.dsn", "data/agent.db")
	viper.SetDefault("store.vector_search", "in_process")
	viper.SetDefault("store.pgvector_dimensions", 0)
	viper.SetDefault("store.paradedb_candidate_multiplier", 4)
	viper.SetDefault("store.paradedb_rrf_k", 60.0)
	viper.SetDefault("store.paradedb_slow_query_threshold_ms", 250)
	viper.SetDefault("store.memory_candidate_limit", 200)
	viper.SetDefault("store.memory_decay_rate", 0.0)
	viper.SetDefault("orchestrator.mode", "eino")
	viper.SetDefault("orchestrator.max_concurrent_tasks", 10)
	viper.SetDefault("orchestrator.run_all_timeout_seconds", 600)
	viper.SetDefault("multiagent.team", "")
	viper.SetDefault("multiagent.runtime", "")
	viper.SetDefault("multiagent.dag_canary_percent", 0)
	viper.SetDefault("approval.ttl_seconds", 86400)
	viper.SetDefault("approval.retention_days", 30)
	viper.SetDefault("answer_pipeline.enabled", true)
	viper.SetDefault("answer_pipeline.enforcement", "observe")
	viper.SetDefault("answer_pipeline.audit_token_reserve", 4000)
	viper.SetDefault("answer_pipeline.stage_timeout_seconds", 20)
	viper.SetDefault("answer_pipeline.parallel_audits", true)
	viper.SetDefault("answer_pipeline.on_required_stage_failure", "partial")
	viper.SetDefault("answer_pipeline.required_stages", []string{"fact_freshness_check", "numeric_consistency_check", "answer_uncertainty_calibrate", "safety_guard_output"})
	viper.SetDefault("answer_pipeline.stage_token_budgets", map[string]int{"citation_verify": 600, "fact_freshness_check": 900, "numeric_consistency_check": 900, "answer_uncertainty_calibrate": 1000, "safety_guard_output": 600})
	viper.SetDefault("llm.provider", llmprovider.OpenAIResponses)
	viper.SetDefault("llm.timeout_seconds", 30)
	viper.SetDefault("llm.readiness_mode", LLMReadinessGateway)
	viper.SetDefault("llm.readiness_cache_ttl_seconds", 10)
	viper.SetDefault("llm.circuit_breaker_failure_threshold", 5)
	viper.SetDefault("llm.circuit_breaker_cooldown_seconds", 30)
	viper.SetDefault("llm.retry_budget_per_minute", 60)
	viper.SetDefault("llm.max_calls_per_task", 0)
	viper.SetDefault("llm.max_estimated_cost_usd_per_task", 0.0)
	viper.SetDefault("llm.context_compression_trace_threshold", 8)
	// context_compression_token_threshold: 0 = disabled by default
	viper.SetDefault("llm.context_compression_token_threshold", 0)
	viper.SetDefault("llm.planner_trace_max_items", 4)
	viper.SetDefault("llm.planner_observation_max_chars", 800)
	viper.SetDefault("llm.planner_evidence_max_items", 8)
	viper.SetDefault("llm.planner_evidence_line_max_chars", 300)
	viper.SetDefault("llm.planner_trace_max_chars", 5000)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.console", true)
	viper.SetDefault("log.file_enabled", true)
	viper.SetDefault("log.access_enabled", true)
	viper.SetDefault("log.directory", "logs")
	viper.SetDefault("log.retention_days", 30)
	viper.SetDefault("telemetry.enabled", true)
	viper.SetDefault("telemetry.endpoint", "127.0.0.1:4318")
	viper.SetDefault("telemetry.environment", "dev")
	viper.SetDefault("telemetry.exporter", "otlp")
	viper.SetDefault("tool.timeout_seconds", 120)
	viper.SetDefault("skill.root", "skills")
	viper.SetDefault("rag.authorization", "")
	viper.SetDefault("rag.tool_name", "search")
	viper.SetDefault("rag.context_mode", "jit")
	viper.SetDefault("rag.jit_search_max_calls", 2)
	viper.SetDefault("rag.jit_retrieval_max_cycles", 2)
	viper.SetDefault("rag.jit_fetch_max_items", 3)
	viper.SetDefault("rag.jit_rag_fetch_max_bytes", 6000)
	viper.SetDefault("rag.jit_memory_fetch_max_bytes", 2000)
	viper.SetDefault("rag.session_recent_task_limit", 5)
	viper.SetDefault("rag.max_prompt_memories", 3)
	viper.SetDefault("rag.max_memory_bytes", 2500)
	viper.SetDefault("rag.max_memory_prompt_bytes", 8000)
	viper.SetDefault("rag.max_raw_fallback_bytes", 4000)
	viper.SetDefault("wiki.url", "")
	viper.SetDefault("wiki.directory", "")
	viper.SetDefault("wiki.authorization_env", "")
	viper.SetDefault("wiki.default_space", "")
	viper.SetDefault("wiki.timeout_seconds", 15)
	viper.SetDefault("wiki.search_top_k", 5)
	viper.SetDefault("wiki.fetch_max_items", 3)
	viper.SetDefault("wiki.fetch_max_bytes", 12000)
	viper.SetDefault("wiki.circuit_breaker_failure_threshold", 3)
	viper.SetDefault("wiki.circuit_breaker_cooldown_seconds", 30)
	viper.SetDefault("wiki.allow_private_network", false)
	viper.SetDefault("wiki.required", false)
	viper.SetDefault("search.url", "https://api.firecrawl.dev/v1/search")
	viper.SetDefault("search.api_key", "")
	viper.SetDefault("langfuse.enabled", false)
	viper.SetDefault("langfuse.host", "https://cloud.langfuse.com")
	viper.SetDefault("langfuse.bootstrap_missing_prompts", false)
	viper.SetDefault("langfuse.bootstrap_failure_policy", "fail")
	viper.SetDefault("langfuse.bootstrap_timeout_seconds", 15)

	// Explicit bindings for standard env variables
	_ = viper.BindEnv("api.addr", "AI_AGENT_API_ADDR")
	_ = viper.BindEnv("api.api_key", "AI_AGENT_API_KEY")
	_ = viper.BindEnv("api.auth.mode", "AI_AGENT_API_AUTH_MODE")
	_ = viper.BindEnv("api.auth.require_tenant_workspace_root", "AI_AGENT_API_REQUIRE_TENANT_WORKSPACE_ROOT")
	_ = viper.BindEnv("approval.ttl_seconds", "AI_AGENT_APPROVAL_TTL_SECONDS")
	_ = viper.BindEnv("approval.retention_days", "AI_AGENT_APPROVAL_RETENTION_DAYS")
	_ = viper.BindEnv("multiagent.team", "AI_AGENT_MULTIAGENT_TEAM")
	_ = viper.BindEnv("multiagent.runtime", "AI_AGENT_MULTIAGENT_RUNTIME")
	_ = viper.BindEnv("multiagent.dag_canary_percent", "AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT")
	_ = viper.BindEnv("api.auth.bearer.validation_mode", "AI_AGENT_API_BEARER_VALIDATION_MODE")
	_ = viper.BindEnv("api.auth.jwt.issuer", "AI_AGENT_API_JWT_ISSUER")
	_ = viper.BindEnv("api.auth.jwt.audience", "AI_AGENT_API_JWT_AUDIENCE")
	_ = viper.BindEnv("api.auth.jwt.jwks_url", "AI_AGENT_API_JWT_JWKS_URL")
	_ = viper.BindEnv("api.auth.jwt.tenant_claim", "AI_AGENT_API_JWT_TENANT_CLAIM")
	_ = viper.BindEnv("api.auth.jwt.require_known_tenant", "AI_AGENT_API_JWT_REQUIRE_KNOWN_TENANT")
	_ = viper.BindEnv("api.auth.jwt.clock_skew_seconds", "AI_AGENT_API_JWT_CLOCK_SKEW_SECONDS")
	_ = viper.BindEnv("api.auth.jwt.jwks_cache_ttl_seconds", "AI_AGENT_API_JWT_JWKS_CACHE_TTL_SECONDS")
	_ = viper.BindEnv("api.auth.jwt.jwks_request_timeout_seconds", "AI_AGENT_API_JWT_JWKS_REQUEST_TIMEOUT_SECONDS")
	_ = viper.BindEnv("api.auth.introspection.url", "AI_AGENT_API_INTROSPECTION_URL")
	_ = viper.BindEnv("api.auth.introspection.tenant_claim", "AI_AGENT_API_INTROSPECTION_TENANT_CLAIM")
	_ = viper.BindEnv("api.auth.introspection.active_claim", "AI_AGENT_API_INTROSPECTION_ACTIVE_CLAIM")
	_ = viper.BindEnv("api.auth.introspection.issuer", "AI_AGENT_API_INTROSPECTION_ISSUER")
	_ = viper.BindEnv("api.auth.introspection.audience", "AI_AGENT_API_INTROSPECTION_AUDIENCE")
	_ = viper.BindEnv("api.auth.introspection.require_expiration", "AI_AGENT_API_INTROSPECTION_REQUIRE_EXPIRATION")
	_ = viper.BindEnv("api.auth.introspection.require_known_tenant", "AI_AGENT_API_INTROSPECTION_REQUIRE_KNOWN_TENANT")
	_ = viper.BindEnv("api.auth.introspection.allow_private_network", "AI_AGENT_API_INTROSPECTION_ALLOW_PRIVATE_NETWORK")
	_ = viper.BindEnv("api.auth.introspection.timeout_seconds", "AI_AGENT_API_INTROSPECTION_TIMEOUT_SECONDS")
	_ = viper.BindEnv("api.auth.introspection.cache_ttl_seconds", "AI_AGENT_API_INTROSPECTION_CACHE_TTL_SECONDS")
	_ = viper.BindEnv("llm.openai_api_key", "OPENAI_API_KEY")
	_ = viper.BindEnv("llm.gemini_api_key", "GEMINI_API_KEY")
	_ = viper.BindEnv("llm.google_api_key", "GOOGLE_API_KEY")
	_ = viper.BindEnv("rag.tool_name", "AI_AGENT_RAG_TOOL_NAME")
	_ = viper.BindEnv("rag.authorization", "AI_AGENT_RAG_AUTHORIZATION")
	_ = viper.BindEnv("wiki.url", "AI_AGENT_WIKI_URL")
	_ = viper.BindEnv("wiki.directory", "AI_AGENT_WIKI_DIRECTORY")
	_ = viper.BindEnv("wiki.authorization_env", "AI_AGENT_WIKI_AUTHORIZATION_ENV")
	_ = viper.BindEnv("wiki.default_space", "AI_AGENT_WIKI_DEFAULT_SPACE")
	_ = viper.BindEnv("search.url", "AI_AGENT_SEARCH_URL")
	_ = viper.BindEnv("search.api_key", "FIRECRAWL_API_KEY")
	_ = viper.BindEnv("langfuse.public_key", "LANGFUSE_PUBLIC_KEY")
	_ = viper.BindEnv("langfuse.secret_key", "LANGFUSE_SECRET_KEY")
	_ = viper.BindEnv("langfuse.host", "LANGFUSE_BASE_URL")
	_ = viper.BindEnv("langfuse.enabled", "LANGFUSE_ENABLED")
	_ = viper.BindEnv("langfuse.bootstrap_missing_prompts", "LANGFUSE_BOOTSTRAP_MISSING_PROMPTS")
	_ = viper.BindEnv("langfuse.bootstrap_failure_policy", "LANGFUSE_BOOTSTRAP_FAILURE_POLICY")
	_ = viper.BindEnv("langfuse.bootstrap_timeout_seconds", "LANGFUSE_BOOTSTRAP_TIMEOUT_SECONDS")
}

// unmarshalConfig reads the current viper state into a fresh Config struct.
// Returns an error if unmarshalling fails; does NOT update globalConfig.
func unmarshalConfig() (*Config, error) {
	var c Config
	if err := viper.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config unmarshal failed: %w", err)
	}
	if c.API.APIKey == "" {
		if envKey := os.Getenv("AI_AGENT_API_KEY"); envKey != "" {
			c.API.APIKey = envKey
		}
	}
	if c.API.Addr == "" {
		if envAddr := os.Getenv("AI_AGENT_API_ADDR"); envAddr != "" {
			c.API.Addr = envAddr
		}
	}
	if c.Search.APIKey == "" {
		if envKey := os.Getenv("AI_AGENT_SEARCH_API_KEY"); envKey != "" {
			c.Search.APIKey = envKey
		}
	}
	if c.RAG.Authorization == "" {
		if envAuth := os.Getenv("AI_AGENT_RAG_AUTHORIZATION"); envAuth != "" {
			c.RAG.Authorization = envAuth
		}
	}
	return &c, nil
}

// LoadConfig loads the configuration once and caches it. Subsequent calls
// return the cached copy. Use Reload() to force a refresh.
func LoadConfig() *Config {
	// Fast path: already initialised.
	mu.RLock()
	if globalConfig != nil {
		defer mu.RUnlock()
		return globalConfig
	}
	mu.RUnlock()

	// Slow path: first load.
	mu.Lock()
	defer mu.Unlock()

	// Double-check after acquiring write lock.
	if globalConfig != nil {
		return globalConfig
	}

	setupViper()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			logger.Warn("no config file found, using defaults and environment variables")
		} else {
			logger.Error("error reading config file", "error", err)
			panic(fmt.Sprintf("fatal config error: %v", err))
		}
	}

	c, err := unmarshalConfig()
	if err != nil {
		logger.Error("config unmarshal failed", "error", err)
		panic(fmt.Sprintf("fatal config error: %v", err))
	}
	if err := c.Validate(); err != nil {
		panic(fmt.Sprintf("fatal config validation error: %v", err))
	}

	globalConfig = c
	configRevision++
	return globalConfig
}

// Get returns the current (possibly hot-updated) configuration snapshot.
// Safe for concurrent use; callers should NOT cache the returned pointer across
// calls — always call Get() at the point of use to pick up live updates.
func Get() *Config {
	return LoadConfig()
}

// Revision returns the monotonically increasing in-process configuration
// revision. It changes after initial load, effective reloads, and test
// overrides, allowing operators to distinguish repeated no-op reload calls
// from an earlier filesystem-watcher update.
func Revision() uint64 {
	mu.RLock()
	defer mu.RUnlock()
	return configRevision
}

// OverrideForTesting replaces the global configuration with a cloned snapshot
// and returns an idempotent restore function. It is intended for tests that
// need a temporary configuration without mutating the shared object returned
// by Get. A stale restore cannot overwrite a newer reload or override.
func OverrideForTesting(mutate func(*Config)) func() {
	if mutate == nil {
		panic("config: nil test override")
	}
	_ = Get()
	mu.Lock()
	previous := globalConfig
	replacement := cloneConfig(previous)
	mutate(replacement)
	globalConfig = replacement
	configRevision++
	revision := configRevision
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			if configRevision == revision {
				globalConfig = previous
				configRevision++
			}
		})
	}
}

func cloneConfig(source *Config) *Config {
	if source == nil {
		return &Config{}
	}
	cloned := *source
	cloned.API.Auth.JWT.AllowedAlgorithms = append([]string(nil), source.API.Auth.JWT.AllowedAlgorithms...)
	cloned.API.Tenants = make(map[string]APITenantConfig, len(source.API.Tenants))
	for tenantID, tenant := range source.API.Tenants {
		tenant.AnswerPipelineRequiredStages = append([]string(nil), tenant.AnswerPipelineRequiredStages...)
		cloned.API.Tenants[tenantID] = tenant
	}
	cloned.LLM.Gateway = cloneLLMEndpoint(source.LLM.Gateway)
	cloned.AnswerPipeline.RequiredStages = append([]string(nil), source.AnswerPipeline.RequiredStages...)
	cloned.AnswerPipeline.StageTokenBudgets = make(map[string]int, len(source.AnswerPipeline.StageTokenBudgets))
	for stage, budget := range source.AnswerPipeline.StageTokenBudgets {
		cloned.AnswerPipeline.StageTokenBudgets[stage] = budget
	}
	cloned.LLM.Scenes = make(map[string]LLMEndpointConfig, len(source.LLM.Scenes))
	for scene, endpoint := range source.LLM.Scenes {
		cloned.LLM.Scenes[scene] = cloneLLMEndpoint(endpoint)
	}
	cloned.MCP.Servers = append([]MCPServerConfig(nil), source.MCP.Servers...)
	return &cloned
}

func cloneLLMEndpoint(source LLMEndpointConfig) LLMEndpointConfig {
	cloned := source
	if source.FallbackScene != nil {
		value := *source.FallbackScene
		cloned.FallbackScene = &value
	}
	if source.MaxRetries != nil {
		value := *source.MaxRetries
		cloned.MaxRetries = &value
	}
	if source.MinRemainingTokens != nil {
		value := *source.MinRemainingTokens
		cloned.MinRemainingTokens = &value
	}
	if source.InputCostPerMillionUSD != nil {
		value := *source.InputCostPerMillionUSD
		cloned.InputCostPerMillionUSD = &value
	}
	if source.OutputCostPerMillionUSD != nil {
		value := *source.OutputCostPerMillionUSD
		cloned.OutputCostPerMillionUSD = &value
	}
	cloned.Routes = make([]LLMRouteRule, len(source.Routes))
	for i, route := range source.Routes {
		cloned.Routes[i] = cloneLLMRouteRule(route)
	}
	return cloned
}

func cloneLLMRouteRule(source LLMRouteRule) LLMRouteRule {
	cloned := source
	cloned.Intents = append([]string(nil), source.Intents...)
	cloned.Complexities = append([]string(nil), source.Complexities...)
	cloned.CostTiers = append([]string(nil), source.CostTiers...)
	cloned.LatencyTiers = append([]string(nil), source.LatencyTiers...)
	cloned.QualityTiers = append([]string(nil), source.QualityTiers...)
	cloneInt := func(value *int) *int {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	cloned.MinRemainingTokens = cloneInt(source.MinRemainingTokens)
	cloned.MaxRemainingTokens = cloneInt(source.MaxRemainingTokens)
	cloned.MinStepCount = cloneInt(source.MinStepCount)
	cloned.MaxStepCount = cloneInt(source.MaxStepCount)
	return cloned
}

// Reload re-reads the configuration file and environment variables, atomically
// replaces the global config, and prints a redacted diff of what changed.
//
// This is the hot-reload path triggered by SIGHUP or the /api/config/reload
// endpoint. On error the existing configuration is preserved unchanged.
func Reload() (*Config, []string, error) {
	mu.Lock()
	defer mu.Unlock()

	// Re-read the config file (picks up any on-disk changes).
	if err := viper.ReadInConfig(); err != nil {
		if _, notFound := err.(viper.ConfigFileNotFoundError); !notFound {
			return nil, nil, fmt.Errorf("config reload: read file failed: %w", err)
		}
		// No config file is not fatal; env-vars still apply.
	}

	newCfg, err := unmarshalConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("config reload: %w", err)
	}
	if err := newCfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("config reload: validation failed: %w", err)
	}

	changes := applyReloadedConfig(newCfg)

	if len(changes) == 0 {
		logger.Info("reload complete, no changes detected")
	} else {
		logger.Info("reload complete", "change_count", len(changes))
		for _, c := range changes {
			logger.Info("config change", "diff", c)
		}
	}
	return globalConfig, changes, nil
}

// applyReloadedConfig installs a validated snapshot while mu is held. A no-op
// refresh replaces the snapshot but does not create a new configuration
// revision. The generic fallback keeps no_changes accurate if a newly added
// field has not yet gained a detailed redacted diff formatter.
func applyReloadedConfig(newCfg *Config) []string {
	changes := diffConfigs(globalConfig, newCfg)
	changed := !reflect.DeepEqual(globalConfig, newCfg)
	globalConfig = newCfg
	if !changed {
		return changes
	}
	configRevision++
	if len(changes) == 0 {
		changes = append(changes, "configuration changed (details unavailable)")
	}
	return changes
}

// Watch registers a viper OnConfigChange hook so that the config is
// automatically hot-reloaded whenever the config file is modified on disk.
// It also starts viper's filesystem watcher goroutine.
//
// Call Watch() once from main() after the first LoadConfig(). Requires that a
// config file was found (viper.ConfigFileUsed() != "").
func Watch() {
	if viper.ConfigFileUsed() == "" {
		logger.Warn("Watch: no config file in use; filesystem watch not started")
		return
	}

	viper.OnConfigChange(func(e fsnotify.Event) {
		logger.Info("config file changed, triggering hot reload")
		if _, changes, err := Reload(); err != nil {
			logger.Error("hot reload failed, keeping previous config", "error", err)
		} else if len(changes) > 0 {
			logger.Info("hot reload applied changes", "change_count", len(changes))
		}
	})
	viper.WatchConfig()
	logger.Info("watching config file for changes", "file", viper.ConfigFileUsed())
}

// ── Diff helper ───────────────────────────────────────────────────────────────

// diffConfigs returns a slice of human-readable change descriptions comparing
// old to new. API Keys are redacted to "***" so they never appear in logs.
func diffConfigs(old, new *Config) []string {
	if old == nil {
		return nil
	}
	var changes []string

	addIf := func(field, o, n string) {
		if o != n {
			// Redact fields whose names suggest they contain secrets.
			if looksLikeSecret(field) {
				o = redact(o)
				n = redact(n)
			}
			changes = append(changes, fmt.Sprintf("%s: %q → %q", field, o, n))
		}
	}
	addIfInt := func(field string, o, n int) {
		if o != n {
			changes = append(changes, fmt.Sprintf("%s: %d → %d", field, o, n))
		}
	}

	// API
	addIf("api.addr", old.API.Addr, new.API.Addr)
	addIf("api.api_key", old.API.APIKey, new.API.APIKey)
	if !reflect.DeepEqual(old.API.Auth, new.API.Auth) {
		changes = append(changes, "api.auth: changed")
	}
	if !reflect.DeepEqual(old.API.Tenants, new.API.Tenants) {
		changes = append(changes, "api.tenants: changed")
	}

	// LLM
	addIf("llm.provider", old.LLM.Provider, new.LLM.Provider)
	addIf("llm.api_key", old.LLM.APIKey, new.LLM.APIKey)
	addIf("llm.openai_api_key", old.LLM.OpenAIAPIKey, new.LLM.OpenAIAPIKey)
	addIf("llm.gemini_api_key", old.LLM.GeminiAPIKey, new.LLM.GeminiAPIKey)
	addIf("llm.google_api_key", old.LLM.GoogleAPIKey, new.LLM.GoogleAPIKey)
	addIf("llm.model", old.LLM.Model, new.LLM.Model)
	addIf("llm.base_url", old.LLM.BaseURL, new.LLM.BaseURL)
	addIfInt("llm.timeout_seconds", old.LLM.TimeoutSeconds, new.LLM.TimeoutSeconds)
	addIf("llm.readiness_mode", old.LLM.ReadinessMode, new.LLM.ReadinessMode)
	addIfInt("llm.readiness_cache_ttl_seconds", old.LLM.ReadinessCacheTTLSeconds, new.LLM.ReadinessCacheTTLSeconds)
	addIfInt("llm.circuit_breaker_failure_threshold", old.LLM.CircuitBreakerFailureThreshold, new.LLM.CircuitBreakerFailureThreshold)
	addIfInt("llm.circuit_breaker_cooldown_seconds", old.LLM.CircuitBreakerCooldownSeconds, new.LLM.CircuitBreakerCooldownSeconds)
	addIfInt("llm.retry_budget_per_minute", old.LLM.RetryBudgetPerMinute, new.LLM.RetryBudgetPerMinute)
	addIfInt("llm.max_calls_per_task", old.LLM.MaxCallsPerTask, new.LLM.MaxCallsPerTask)
	if old.LLM.MaxEstimatedCostUSDPerTask != new.LLM.MaxEstimatedCostUSDPerTask {
		changes = append(changes, fmt.Sprintf("llm.max_estimated_cost_usd_per_task: %g -> %g", old.LLM.MaxEstimatedCostUSDPerTask, new.LLM.MaxEstimatedCostUSDPerTask))
	}
	addIfInt("llm.context_compression_trace_threshold", old.LLM.ContextCompressionTraceThreshold, new.LLM.ContextCompressionTraceThreshold)
	addIfInt("llm.context_compression_token_threshold", old.LLM.ContextCompressionTokenThreshold, new.LLM.ContextCompressionTokenThreshold)
	addIfInt("llm.planner_trace_max_items", old.LLM.PlannerTraceMaxItems, new.LLM.PlannerTraceMaxItems)
	addIfInt("llm.planner_observation_max_chars", old.LLM.PlannerObservationMaxChars, new.LLM.PlannerObservationMaxChars)
	addIfInt("llm.planner_evidence_max_items", old.LLM.PlannerEvidenceMaxItems, new.LLM.PlannerEvidenceMaxItems)
	addIfInt("llm.planner_evidence_line_max_chars", old.LLM.PlannerEvidenceLineMaxChars, new.LLM.PlannerEvidenceLineMaxChars)
	addIfInt("llm.planner_trace_max_chars", old.LLM.PlannerTraceMaxChars, new.LLM.PlannerTraceMaxChars)
	addIf("llm.gateway.provider", old.LLM.Gateway.Provider, new.LLM.Gateway.Provider)
	addIf("llm.gateway.api_key", old.LLM.Gateway.APIKey, new.LLM.Gateway.APIKey)
	addIf("llm.gateway.model", old.LLM.Gateway.Model, new.LLM.Gateway.Model)
	addIf("llm.gateway.base_url", old.LLM.Gateway.BaseURL, new.LLM.Gateway.BaseURL)
	addIfInt("llm.gateway.timeout_seconds", old.LLM.Gateway.TimeoutSeconds, new.LLM.Gateway.TimeoutSeconds)
	if !reflect.DeepEqual(old.LLM.Gateway.InputCostPerMillionUSD, new.LLM.Gateway.InputCostPerMillionUSD) || !reflect.DeepEqual(old.LLM.Gateway.OutputCostPerMillionUSD, new.LLM.Gateway.OutputCostPerMillionUSD) {
		changes = append(changes, "llm.gateway.cost_rates: changed")
	}
	if !reflect.DeepEqual(old.LLM.Scenes, new.LLM.Scenes) {
		changes = append(changes, "llm.scenes: changed")
	}

	// Store (DSN may contain a password)
	addIf("store.type", old.Store.Type, new.Store.Type)
	addIf("store.dsn", old.Store.DSN, new.Store.DSN)
	addIf("store.vector_search", old.Store.VectorSearch, new.Store.VectorSearch)
	addIfInt("store.pgvector_dimensions", old.Store.PGVectorDimensions, new.Store.PGVectorDimensions)
	addIfInt("store.paradedb_candidate_multiplier", old.Store.ParadeDBCandidateMultiplier, new.Store.ParadeDBCandidateMultiplier)
	if old.Store.ParadeDBRRFK != new.Store.ParadeDBRRFK {
		changes = append(changes, fmt.Sprintf("store.paradedb_rrf_k: %g → %g", old.Store.ParadeDBRRFK, new.Store.ParadeDBRRFK))
	}
	addIfInt("store.paradedb_slow_query_threshold_ms", old.Store.ParadeDBSlowQueryThresholdMS, new.Store.ParadeDBSlowQueryThresholdMS)
	addIfInt("store.memory_candidate_limit", old.Store.MemoryCandidateLimit, new.Store.MemoryCandidateLimit)
	if old.Store.MemoryDecayRate != new.Store.MemoryDecayRate {
		changes = append(changes, fmt.Sprintf("store.memory_decay_rate: %g → %g", old.Store.MemoryDecayRate, new.Store.MemoryDecayRate))
	}

	// Orchestrator
	addIf("orchestrator.mode", old.Orchestrator.Mode, new.Orchestrator.Mode)
	addIfInt("orchestrator.max_concurrent_tasks", old.Orchestrator.MaxConcurrentTasks, new.Orchestrator.MaxConcurrentTasks)
	addIfInt("orchestrator.run_all_timeout_seconds", old.Orchestrator.RunAllTimeoutSeconds, new.Orchestrator.RunAllTimeoutSeconds)
	addIf("multiagent.team", old.MultiAgent.Team, new.MultiAgent.Team)
	addIf("multiagent.runtime", old.MultiAgent.Runtime, new.MultiAgent.Runtime)
	addIfInt("multiagent.dag_canary_percent", old.MultiAgent.DAGCanaryPercent, new.MultiAgent.DAGCanaryPercent)
	addIfInt("approval.ttl_seconds", old.Approval.TTLSeconds, new.Approval.TTLSeconds)
	addIfInt("approval.retention_days", old.Approval.RetentionDays, new.Approval.RetentionDays)
	if !reflect.DeepEqual(old.AnswerPipeline, new.AnswerPipeline) {
		changes = append(changes, "answer_pipeline: changed")
	}

	// RAG
	addIf("rag.search_url", old.RAG.SearchURL, new.RAG.SearchURL)
	addIf("rag.search_method", old.RAG.SearchMethod, new.RAG.SearchMethod)
	addIf("rag.authorization", old.RAG.Authorization, new.RAG.Authorization)
	addIf("rag.tool_name", old.RAG.ToolName, new.RAG.ToolName)
	addIf("rag.context_mode", old.RAG.ContextMode, new.RAG.ContextMode)
	addIfInt("rag.jit_search_max_calls", old.RAG.JITSearchMaxCalls, new.RAG.JITSearchMaxCalls)
	addIfInt("rag.jit_retrieval_max_cycles", old.RAG.JITRetrievalMaxCycles, new.RAG.JITRetrievalMaxCycles)
	addIfInt("rag.jit_fetch_max_items", old.RAG.JITFetchMaxItems, new.RAG.JITFetchMaxItems)
	addIfInt("rag.jit_rag_fetch_max_bytes", old.RAG.JITRAGFetchMaxBytes, new.RAG.JITRAGFetchMaxBytes)
	addIfInt("rag.jit_memory_fetch_max_bytes", old.RAG.JITMemoryFetchMaxBytes, new.RAG.JITMemoryFetchMaxBytes)
	addIfInt("rag.session_recent_task_limit", old.RAG.SessionRecentTaskLimit, new.RAG.SessionRecentTaskLimit)
	addIfInt("rag.max_prompt_memories", old.RAG.MaxPromptMemories, new.RAG.MaxPromptMemories)
	addIfInt("rag.max_memory_bytes", old.RAG.MaxMemoryBytes, new.RAG.MaxMemoryBytes)
	addIfInt("rag.max_memory_prompt_bytes", old.RAG.MaxMemoryPromptBytes, new.RAG.MaxMemoryPromptBytes)
	addIfInt("rag.max_raw_fallback_bytes", old.RAG.MaxRawFallbackBytes, new.RAG.MaxRawFallbackBytes)

	// MCP credentials are referenced by environment-variable name, never by
	// value, so comparing the declarative server list is safe.
	if !reflect.DeepEqual(old.MCP.Servers, new.MCP.Servers) {
		changes = append(changes, "mcp.servers: changed")
	}
	if !reflect.DeepEqual(old.Wiki, new.Wiki) {
		changes = append(changes, "wiki: changed (restart required)")
	}

	// Search
	addIf("search.url", old.Search.URL, new.Search.URL)
	addIf("search.api_key", old.Search.APIKey, new.Search.APIKey)

	// Embedding
	addIf("embedding.model", old.Embedding.Model, new.Embedding.Model)

	// Tool / Log / Skill / Telemetry
	addIfInt("tool.timeout_seconds", old.Tool.TimeoutSeconds, new.Tool.TimeoutSeconds)
	addIf("log.level", old.Log.Level, new.Log.Level)
	if old.Log.Console != new.Log.Console {
		changes = append(changes, fmt.Sprintf("log.console: %t → %t", old.Log.Console, new.Log.Console))
	}
	if old.Log.FileEnabled != new.Log.FileEnabled {
		changes = append(changes, fmt.Sprintf("log.file_enabled: %t → %t", old.Log.FileEnabled, new.Log.FileEnabled))
	}
	if old.Log.AccessEnabled != new.Log.AccessEnabled {
		changes = append(changes, fmt.Sprintf("log.access_enabled: %t → %t", old.Log.AccessEnabled, new.Log.AccessEnabled))
	}
	addIf("log.directory", old.Log.Directory, new.Log.Directory)
	addIfInt("log.retention_days", old.Log.RetentionDays, new.Log.RetentionDays)
	addIf("skill.root", old.Skill.Root, new.Skill.Root)
	if old.Telemetry.Enabled != new.Telemetry.Enabled {
		changes = append(changes, fmt.Sprintf("telemetry.enabled: %t → %t", old.Telemetry.Enabled, new.Telemetry.Enabled))
	}
	addIf("telemetry.endpoint", old.Telemetry.Endpoint, new.Telemetry.Endpoint)
	addIf("telemetry.environment", old.Telemetry.Environment, new.Telemetry.Environment)
	addIf("telemetry.exporter", old.Telemetry.Exporter, new.Telemetry.Exporter)
	if old.Langfuse.Enabled != new.Langfuse.Enabled {
		changes = append(changes, fmt.Sprintf("langfuse.enabled: %t → %t", old.Langfuse.Enabled, new.Langfuse.Enabled))
	}
	addIf("langfuse.host", old.Langfuse.Host, new.Langfuse.Host)
	if old.Langfuse.BootstrapMissingPrompts != new.Langfuse.BootstrapMissingPrompts {
		changes = append(changes, fmt.Sprintf("langfuse.bootstrap_missing_prompts: %t → %t", old.Langfuse.BootstrapMissingPrompts, new.Langfuse.BootstrapMissingPrompts))
	}
	addIf("langfuse.bootstrap_failure_policy", old.Langfuse.BootstrapFailurePolicy, new.Langfuse.BootstrapFailurePolicy)
	addIfInt("langfuse.bootstrap_timeout_seconds", old.Langfuse.BootstrapTimeoutSeconds, new.Langfuse.BootstrapTimeoutSeconds)

	return changes
}

func looksLikeSecret(field string) bool {
	lower := strings.ToLower(field)
	return strings.Contains(lower, "key") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "password") ||
		strings.Contains(lower, "dsn") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "auth") ||
		strings.Contains(lower, "authorization")
}

func redact(v string) string {
	if v == "" {
		return ""
	}
	return "***"
}

// ── Helper methods ────────────────────────────────────────────────────────────

// Helper methods to resolve dynamic fallback logic for API Keys and Providers
func (c *Config) ResolveLLMProvider() string {
	provider := c.LLM.Provider
	if provider == "" || provider == llmprovider.OpenAIResponses { // if not explicitly overridden by env
		if c.LLM.OpenAIAPIKey != "" {
			return llmprovider.OpenAIResponses
		}
		if c.LLM.GeminiAPIKey != "" || c.LLM.GoogleAPIKey != "" {
			return llmprovider.Gemini
		}
	}
	return provider
}

func (c *Config) ResolveLLMAPIKey(provider string) string {
	spec, registered := llmprovider.Lookup(provider)
	if !registered {
		return c.LLM.APIKey
	}
	switch spec.CredentialFamily {
	case llmprovider.CredentialOpenAI:
		if c.LLM.OpenAIAPIKey != "" {
			return c.LLM.OpenAIAPIKey
		}
		return c.LLM.APIKey
	case llmprovider.CredentialGemini:
		if c.LLM.GeminiAPIKey != "" {
			return c.LLM.GeminiAPIKey
		}
		if c.LLM.GoogleAPIKey != "" {
			return c.LLM.GoogleAPIKey
		}
		return c.LLM.APIKey
	default:
		return c.LLM.APIKey
	}
}

func (c *Config) ResolveLLMModel(provider string) string {
	if c.LLM.Model != "" {
		return c.LLM.Model
	}
	return defaultLLMModel(provider)
}

func defaultLLMModel(provider string) string {
	if spec, ok := llmprovider.Lookup(provider); ok {
		return spec.DefaultModel
	}
	return ""
}

func (c *Config) ResolveLLMBaseURL(provider string) string {
	if c.LLM.BaseURL != "" {
		return c.LLM.BaseURL
	}
	return defaultLLMBaseURL(provider)
}

func defaultLLMBaseURL(provider string) string {
	if spec, ok := llmprovider.Lookup(provider); ok {
		return spec.DefaultBaseURL
	}
	return ""
}

// ResolveLLMScene resolves a call-site profile while preserving compatibility
// with the legacy flat llm configuration.
func (c *Config) ResolveLLMScene(scene string) ResolvedLLMConfig {
	provider := c.ResolveLLMProvider()
	apiKey := c.ResolveLLMAPIKey(provider)
	model := c.ResolveLLMModel(provider)
	baseURL := c.ResolveLLMBaseURL(provider)
	timeout := c.LLM.TimeoutSeconds
	fallbackScene := ""
	maxRetries := 0
	minRemainingTokens := 0
	inputCostPerMillionUSD := 0.0
	outputCostPerMillionUSD := 0.0

	apply := func(v LLMEndpointConfig) {
		providerChanged := v.Provider != "" && v.Provider != provider
		if v.Provider != "" {
			provider = v.Provider
		}
		if v.APIKey != "" {
			apiKey = v.APIKey
		} else if providerChanged {
			apiKey = c.ResolveLLMAPIKey(provider)
		}
		if v.Model != "" {
			model = v.Model
		} else if providerChanged {
			model = defaultLLMModel(provider)
		}
		if v.BaseURL != "" {
			baseURL = v.BaseURL
		} else if providerChanged {
			baseURL = defaultLLMBaseURL(provider)
		}
		if v.TimeoutSeconds > 0 {
			timeout = v.TimeoutSeconds
		}
		if v.FallbackScene != nil {
			fallbackScene = *v.FallbackScene
		}
		if v.MaxRetries != nil {
			maxRetries = *v.MaxRetries
		}
		if v.MinRemainingTokens != nil {
			minRemainingTokens = *v.MinRemainingTokens
		}
		if v.InputCostPerMillionUSD != nil {
			inputCostPerMillionUSD = *v.InputCostPerMillionUSD
		}
		if v.OutputCostPerMillionUSD != nil {
			outputCostPerMillionUSD = *v.OutputCostPerMillionUSD
		}
	}

	apply(c.LLM.Gateway)
	if v, ok := c.LLM.Scenes[scene]; ok {
		apply(v)
	}
	if scene == LLMSceneEmbedding {
		if c.Embedding.Model != "" {
			model = c.Embedding.Model
		} else if endpoint, configured := c.LLM.Scenes[scene]; !configured || endpoint.Model == "" {
			if spec, ok := llmprovider.Lookup(provider); ok && spec.DefaultEmbeddingModel != "" {
				model = spec.DefaultEmbeddingModel
			}
		}
	}
	if timeout <= 0 {
		timeout = 30
	}
	return ResolvedLLMConfig{Provider: provider, APIKey: apiKey, Model: model, BaseURL: baseURL, TimeoutSeconds: timeout, FallbackScene: fallbackScene, MaxRetries: maxRetries, MinRemainingTokens: minRemainingTokens, InputCostPerMillionUSD: inputCostPerMillionUSD, OutputCostPerMillionUSD: outputCostPerMillionUSD}
}

func (c *Config) ResolveLLMRoutedScene(scene string, hints LLMRoutingHints) string {
	current := scene
	visited := make(map[string]bool)
	for current != "" && !visited[current] {
		visited[current] = true
		endpoint, configured := c.LLM.Scenes[current]
		if !configured {
			break
		}
		target := ""
		for _, rule := range endpoint.Routes {
			if routeMatches(rule, hints) {
				target = rule.TargetScene
				break
			}
		}
		if target == "" {
			break
		}
		current = target
	}
	return current
}

// ValidateLLMCostBudgetCoverage ensures every configured generation scene has
// pricing, so a task cost limit cannot be bypassed by an unpriced call.
func (c *Config) ValidateLLMCostBudgetCoverage() error {
	scenes := map[string]struct{}{LLMSceneTaskPlanner: {}}
	for scene := range c.LLM.Scenes {
		if scene != LLMSceneEmbedding {
			scenes[scene] = struct{}{}
		}
	}
	for scene := range scenes {
		resolved := c.ResolveLLMScene(scene)
		if resolved.InputCostPerMillionUSD == 0 && resolved.OutputCostPerMillionUSD == 0 {
			return fmt.Errorf("llm scene %q requires input or output pricing when a task cost budget is enabled; configure llm.gateway or llm.scenes.%s pricing, or set llm_cost_budget_usd to 0", scene, scene)
		}
	}
	return nil
}

func routeMatches(rule LLMRouteRule, hints LLMRoutingHints) bool {
	if rule.MinRemainingTokens != nil {
		if !hints.HasRemainingTokens || hints.RemainingTokens < *rule.MinRemainingTokens {
			return false
		}
	}
	if rule.MaxRemainingTokens != nil {
		if !hints.HasRemainingTokens || hints.RemainingTokens > *rule.MaxRemainingTokens {
			return false
		}
	}
	if rule.MinStepCount != nil && hints.StepCount < *rule.MinStepCount {
		return false
	}
	if rule.MaxStepCount != nil && hints.StepCount > *rule.MaxStepCount {
		return false
	}
	if !routeStringMatches(rule.Intents, hints.Intent) || !routeStringMatches(rule.Complexities, hints.Complexity) || !routeStringMatches(rule.CostTiers, hints.CostTier) || !routeStringMatches(rule.LatencyTiers, hints.LatencyTier) || !routeStringMatches(rule.QualityTiers, hints.QualityTier) {
		return false
	}
	return true
}

func routeStringMatches(allowed []string, actual string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == actual {
			return true
		}
	}
	return false
}

func (c *Config) validateJWTAuth() error {
	jwtConfig := c.API.Auth.JWT
	if strings.TrimSpace(jwtConfig.Issuer) == "" || strings.TrimSpace(jwtConfig.Audience) == "" || strings.TrimSpace(jwtConfig.JWKSURL) == "" || strings.TrimSpace(jwtConfig.TenantClaim) == "" {
		return fmt.Errorf("api.auth.jwt issuer, audience, jwks_url, and tenant_claim are required")
	}
	parsedJWKSURL, err := url.Parse(jwtConfig.JWKSURL)
	if err != nil || parsedJWKSURL.Host == "" || parsedJWKSURL.User != nil || parsedJWKSURL.Fragment != "" || (parsedJWKSURL.Scheme != "https" && parsedJWKSURL.Scheme != "http") {
		return fmt.Errorf("api.auth.jwt.jwks_url must be an absolute http or https URL without userinfo or fragment")
	}
	if len(jwtConfig.AllowedAlgorithms) == 0 {
		return fmt.Errorf("api.auth.jwt.allowed_algorithms must not be empty")
	}
	for _, algorithm := range jwtConfig.AllowedAlgorithms {
		switch strings.ToUpper(strings.TrimSpace(algorithm)) {
		case "RS256", "RS384", "RS512":
		default:
			return fmt.Errorf("api.auth.jwt.allowed_algorithms contains unsupported algorithm %q", algorithm)
		}
	}
	if jwtConfig.ClockSkewSeconds < 0 || jwtConfig.JWKSCacheTTLSeconds <= 0 || jwtConfig.JWKSRequestTimeoutSeconds <= 0 {
		return fmt.Errorf("api.auth.jwt clock skew must be >= 0 and JWKS cache/request timeouts must be > 0")
	}
	return nil
}

func (c *Config) validateIntrospectionAuth() error {
	introspection := c.API.Auth.Introspection
	if strings.TrimSpace(introspection.URL) == "" || strings.TrimSpace(introspection.TenantClaim) == "" || strings.TrimSpace(introspection.ActiveClaim) == "" {
		return fmt.Errorf("api.auth.introspection url, tenant_claim, and active_claim are required")
	}
	parsedURL, err := url.Parse(introspection.URL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" {
		return fmt.Errorf("api.auth.introspection.url must be an absolute https URL without userinfo or fragment")
	}
	if introspection.TimeoutSeconds <= 0 || introspection.CacheTTLSeconds < 0 {
		return fmt.Errorf("api.auth.introspection timeout_seconds must be > 0 and cache_ttl_seconds must be >= 0")
	}
	return nil
}

// Validate rejects configuration that would otherwise fail only on the first
// LLM request. API keys are intentionally not required here because Ollama and
// LiteLLM may run without authentication.
func (c *Config) Validate() error {
	switch strings.ToLower(strings.TrimSpace(c.Orchestrator.Mode)) {
	case "", "eino", "legacy", "adk", "step", "multiagent":
	default:
		return fmt.Errorf("orchestrator.mode must be one of eino, legacy, adk, step, or multiagent")
	}
	switch strings.ToLower(strings.TrimSpace(c.MultiAgent.Runtime)) {
	case "", "legacy", "dag":
	default:
		return fmt.Errorf("multiagent.runtime must be legacy or dag")
	}
	if c.MultiAgent.DAGCanaryPercent < 0 || c.MultiAgent.DAGCanaryPercent > 100 {
		return fmt.Errorf("multiagent.dag_canary_percent must be between 0 and 100")
	}
	switch strings.ToLower(strings.TrimSpace(c.Store.VectorSearch)) {
	case "", "in_process", "pgvector", "paradedb":
	default:
		return fmt.Errorf("store.vector_search must be one of in_process, pgvector, or paradedb")
	}
	if c.Store.PGVectorDimensions < 0 || c.Store.ParadeDBCandidateMultiplier < 0 || c.Store.ParadeDBRRFK < 0 || c.Store.ParadeDBSlowQueryThresholdMS < 0 || c.Store.MemoryCandidateLimit < 0 || c.Store.MemoryDecayRate < 0 {
		return fmt.Errorf("store pgvector_dimensions, ParadeDB ranking/slow-query settings, memory_candidate_limit, and memory_decay_rate must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(c.Langfuse.BootstrapFailurePolicy)) {
	case "", "fail", "warn":
	default:
		return fmt.Errorf("langfuse.bootstrap_failure_policy must be fail or warn")
	}
	if c.Langfuse.BootstrapTimeoutSeconds < 0 {
		return fmt.Errorf("langfuse.bootstrap_timeout_seconds must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(c.AnswerPipeline.Enforcement)) {
	case "", "observe", "advisory", "strict":
	default:
		return fmt.Errorf("answer_pipeline.enforcement must be one of observe, advisory, or strict")
	}
	if c.AnswerPipeline.AuditTokenReserve < 0 || c.AnswerPipeline.StageTimeoutSeconds < 0 {
		return fmt.Errorf("answer_pipeline budgets and timeout must be >= 0")
	}
	for stage, budget := range c.AnswerPipeline.StageTokenBudgets {
		if strings.TrimSpace(stage) == "" || budget < 0 {
			return fmt.Errorf("answer_pipeline stage token budgets must use non-empty names and non-negative values")
		}
		if !knownAnswerPipelineStage(stage) {
			return fmt.Errorf("answer_pipeline.stage_token_budgets contains unknown stage %q", stage)
		}
	}
	seenRequiredStages := make(map[string]bool, len(c.AnswerPipeline.RequiredStages))
	for _, stage := range c.AnswerPipeline.RequiredStages {
		if !knownAnswerPipelineStage(stage) {
			return fmt.Errorf("answer_pipeline.required_stages contains unknown stage %q", stage)
		}
		if seenRequiredStages[stage] {
			return fmt.Errorf("answer_pipeline.required_stages contains duplicate stage %q", stage)
		}
		seenRequiredStages[stage] = true
	}
	switch strings.ToLower(strings.TrimSpace(c.AnswerPipeline.OnRequiredStageFailure)) {
	case "", "partial", "failed":
	default:
		return fmt.Errorf("answer_pipeline.on_required_stage_failure must be partial or failed")
	}
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log.level must be one of debug, info, warn, or error")
	}
	// A completely zero-valued Log section is accepted for callers that build
	// Config directly; configs loaded through Viper always receive defaults.
	logConfigured := c.Log.Level != "" || c.Log.Directory != "" || c.Log.RetentionDays != 0
	if logConfigured && !c.Log.Console && !c.Log.FileEnabled {
		return fmt.Errorf("at least one of log.console or log.file_enabled must be enabled")
	}
	if (c.Log.FileEnabled || c.Log.AccessEnabled) && strings.TrimSpace(c.Log.Directory) == "" {
		return fmt.Errorf("log.directory must not be empty when file or access logging is enabled")
	}
	if c.Log.RetentionDays < 0 {
		return fmt.Errorf("log.retention_days must be >= 0")
	}
	if c.Approval.TTLSeconds < 0 {
		return fmt.Errorf("approval.ttl_seconds must be >= 0")
	}
	if c.Approval.RetentionDays < 0 {
		return fmt.Errorf("approval.retention_days must be >= 0")
	}
	if c.RAG.MaxPromptMemories < 0 || c.RAG.MaxMemoryBytes < 0 || c.RAG.MaxMemoryPromptBytes < 0 || c.RAG.MaxRawFallbackBytes < 0 || c.RAG.SessionRecentTaskLimit < 0 {
		return fmt.Errorf("rag prompt budget values must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(c.RAG.ContextMode)) {
	case "", "jit", "prefetch":
	default:
		return fmt.Errorf("rag.context_mode must be one of jit or prefetch")
	}
	if c.RAG.JITSearchMaxCalls < 0 || c.RAG.JITRetrievalMaxCycles < 0 || c.RAG.JITFetchMaxItems < 0 || c.RAG.JITRAGFetchMaxBytes < 0 || c.RAG.JITMemoryFetchMaxBytes < 0 {
		return fmt.Errorf("rag JIT limits must be >= 0")
	}
	seenMCPNames := make(map[string]bool, len(c.MCP.Servers))
	for i, server := range c.MCP.Servers {
		if server.Disabled {
			continue
		}
		name := strings.TrimSpace(server.Name)
		if name == "" {
			return fmt.Errorf("mcp.servers[%d].name must not be empty", i)
		}
		nameHasAlnum := false
		for _, r := range name {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
				return fmt.Errorf("mcp server name %q may contain only letters, digits, '_' and '-'", name)
			}
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				nameHasAlnum = true
			}
		}
		if !nameHasAlnum {
			return fmt.Errorf("mcp server name %q must contain a letter or digit", name)
		}
		canonicalName := strings.ToLower(name)
		if seenMCPNames[canonicalName] {
			return fmt.Errorf("mcp server name %q is duplicated", name)
		}
		seenMCPNames[canonicalName] = true
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("mcp server %q url must not be empty", name)
		}
		if prefix := strings.TrimSpace(server.ToolPrefix); prefix != "" {
			prefixHasAlnum := false
			for _, r := range prefix {
				if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
					return fmt.Errorf("mcp server %q tool_prefix may contain only letters, digits, '_' and '-'", name)
				}
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
					prefixHasAlnum = true
				}
			}
			if !prefixHasAlnum {
				return fmt.Errorf("mcp server %q tool_prefix must contain a letter or digit", name)
			}
		}
		if server.TimeoutSeconds < 0 {
			return fmt.Errorf("mcp server %q timeout_seconds must be >= 0", name)
		}
		if server.MaxTools < 0 {
			return fmt.Errorf("mcp server %q max_tools must be >= 0", name)
		}
		switch strings.ToLower(strings.TrimSpace(server.RiskLevel)) {
		case "", "low", "high":
		default:
			return fmt.Errorf("mcp server %q risk_level must be low or high", name)
		}
	}
	if strings.TrimSpace(c.Wiki.URL) != "" && strings.TrimSpace(c.Wiki.Directory) != "" {
		return fmt.Errorf("wiki.url and wiki.directory are mutually exclusive")
	}
	if c.Wiki.Required && strings.TrimSpace(c.Wiki.URL) == "" && strings.TrimSpace(c.Wiki.Directory) == "" {
		return fmt.Errorf("wiki.url or wiki.directory must not be empty when wiki.required is true")
	}
	if c.Wiki.TimeoutSeconds < 0 || c.Wiki.SearchTopK < 0 || c.Wiki.FetchMaxItems < 0 || c.Wiki.FetchMaxBytes < 0 || c.Wiki.CircuitBreakerFailureThreshold < 0 || c.Wiki.CircuitBreakerCooldownSeconds < 0 {
		return fmt.Errorf("wiki timeout and retrieval limits must be >= 0")
	}
	if c.Wiki.SearchTopK > 10 {
		return fmt.Errorf("wiki.search_top_k must not exceed 10")
	}
	if c.Wiki.FetchMaxItems > 10 {
		return fmt.Errorf("wiki.fetch_max_items must not exceed 10")
	}
	if c.LLM.PlannerTraceMaxItems < 0 || c.LLM.PlannerObservationMaxChars < 0 || c.LLM.PlannerEvidenceMaxItems < 0 || c.LLM.PlannerEvidenceLineMaxChars < 0 || c.LLM.PlannerTraceMaxChars < 0 {
		return fmt.Errorf("planner trace budget values must be >= 0")
	}
	authMode := strings.ToLower(strings.TrimSpace(c.API.Auth.Mode))
	if authMode == "" {
		authMode = "api_key"
	}
	switch authMode {
	case "api_key":
	case "jwt":
		if err := c.validateJWTAuth(); err != nil {
			return err
		}
	case "introspection":
		if err := c.validateIntrospectionAuth(); err != nil {
			return err
		}
	case "hybrid":
		switch strings.ToLower(strings.TrimSpace(c.API.Auth.Bearer.ValidationMode)) {
		case "", "jwks":
			if err := c.validateJWTAuth(); err != nil {
				return err
			}
		case "introspection":
			if err := c.validateIntrospectionAuth(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("api.auth.bearer.validation_mode must be jwks or introspection")
		}
	default:
		return fmt.Errorf("api.auth.mode must be one of api_key, jwt, introspection, or hybrid")
	}
	seenTenantKeys := make(map[string]string, len(c.API.Tenants))
	for tenantID, tenant := range c.API.Tenants {
		if strings.TrimSpace(tenantID) == "" {
			return fmt.Errorf("api tenant id must not be empty")
		}
		if strings.TrimSpace(tenant.APIKey) == "" && authMode == "api_key" {
			return fmt.Errorf("api tenant %q api_key must not be empty in api_key mode", tenantID)
		}
		if tenant.DailyLLMCallBudget < 0 || tenant.DailyLLMCostBudgetUSD < 0 {
			return fmt.Errorf("api tenant %q budgets must be >= 0", tenantID)
		}
		if c.API.Auth.RequireTenantWorkspaceRoot && !tenant.Admin && strings.TrimSpace(tenant.WorkspaceRoot) == "" {
			return fmt.Errorf("api tenant %q workspace_root is required when api.auth.require_tenant_workspace_root is true", tenantID)
		}
		if tenant.APIKey != "" {
			if previous, exists := seenTenantKeys[tenant.APIKey]; exists {
				return fmt.Errorf("api tenants %q and %q use the same api_key", previous, tenantID)
			}
			if c.API.APIKey != "" && tenant.APIKey == c.API.APIKey {
				return fmt.Errorf("api tenant %q duplicates api.api_key", tenantID)
			}
			seenTenantKeys[tenant.APIKey] = tenantID
		}
		if tenant.DailyLLMCostBudgetUSD > 0 {
			if err := c.ValidateLLMCostBudgetCoverage(); err != nil {
				return err
			}
		}
		switch strings.ToLower(strings.TrimSpace(tenant.AnswerPipelineEnforcement)) {
		case "", "observe", "advisory", "strict":
		default:
			return fmt.Errorf("api tenant %q answer_pipeline_enforcement must be observe, advisory, or strict", tenantID)
		}
		seenStages := make(map[string]bool, len(tenant.AnswerPipelineRequiredStages))
		for _, stage := range tenant.AnswerPipelineRequiredStages {
			if !knownAnswerPipelineStage(stage) {
				return fmt.Errorf("api tenant %q contains unknown answer pipeline stage %q", tenantID, stage)
			}
			if seenStages[stage] {
				return fmt.Errorf("api tenant %q contains duplicate answer pipeline stage %q", tenantID, stage)
			}
			seenStages[stage] = true
		}
	}
	switch c.ResolveLLMReadinessMode() {
	case LLMReadinessConfigOnly, LLMReadinessGateway, LLMReadinessInference:
	default:
		return fmt.Errorf("llm.readiness_mode must be one of %q, %q, or %q", LLMReadinessConfigOnly, LLMReadinessGateway, LLMReadinessInference)
	}
	if c.LLM.ReadinessCacheTTLSeconds < 0 {
		return fmt.Errorf("llm.readiness_cache_ttl_seconds must be >= 0")
	}
	if len(c.LLM.Gateway.Routes) > 0 {
		return fmt.Errorf("llm.gateway.routes is not supported; configure routes on a named scene")
	}
	if c.LLM.ContextCompressionTraceThreshold < 0 {
		return fmt.Errorf("llm.context_compression_trace_threshold must be >= 0")
	}
	if c.LLM.ContextCompressionTokenThreshold < 0 {
		return fmt.Errorf("llm.context_compression_token_threshold must be >= 0")
	}
	if c.LLM.CircuitBreakerFailureThreshold < 0 || c.LLM.CircuitBreakerCooldownSeconds < 0 || c.LLM.RetryBudgetPerMinute < 0 || c.LLM.MaxCallsPerTask < 0 || c.LLM.MaxEstimatedCostUSDPerTask < 0 {
		return fmt.Errorf("llm resilience and task budget values must be >= 0")
	}
	if c.LLM.MaxEstimatedCostUSDPerTask > 0 {
		if err := c.ValidateLLMCostBudgetCoverage(); err != nil {
			return err
		}
	}
	validatePolicy := func(name string, endpoint LLMEndpointConfig) error {
		if (endpoint.MaxRetries != nil && *endpoint.MaxRetries < 0) || (endpoint.MinRemainingTokens != nil && *endpoint.MinRemainingTokens < 0) {
			return fmt.Errorf("%s retry and token policy values must be >= 0", name)
		}
		if (endpoint.InputCostPerMillionUSD != nil && *endpoint.InputCostPerMillionUSD < 0) || (endpoint.OutputCostPerMillionUSD != nil && *endpoint.OutputCostPerMillionUSD < 0) {
			return fmt.Errorf("%s LLM cost values must be >= 0", name)
		}
		return nil
	}
	if err := validatePolicy("llm.gateway", c.LLM.Gateway); err != nil {
		return err
	}
	visionScenes := map[string]bool{}
	var markVisionScene func(string)
	markVisionScene = func(scene string) {
		if visionScenes[scene] {
			return
		}
		visionScenes[scene] = true
		endpoint, ok := c.LLM.Scenes[scene]
		if !ok {
			return
		}
		if fallback := c.ResolveLLMScene(scene).FallbackScene; fallback != "" {
			markVisionScene(fallback)
		}
		for _, route := range endpoint.Routes {
			markVisionScene(route.TargetScene)
		}
	}
	if _, configured := c.LLM.Scenes[LLMSceneVisionAnalyzer]; configured {
		markVisionScene(LLMSceneVisionAnalyzer)
	}
	check := func(scene string) error {
		resolved := c.ResolveLLMScene(scene)
		spec, registered := llmprovider.Lookup(resolved.Provider)
		if !registered {
			return fmt.Errorf("llm scene %q has unsupported provider %q", scene, resolved.Provider)
		}
		requiredCapability := llmprovider.CapabilityStructuredOutput
		capabilityName := "structured output"
		if scene == LLMSceneEmbedding {
			requiredCapability = llmprovider.CapabilityEmbedding
			capabilityName = "embedding"
		}
		if !spec.Supports(requiredCapability) {
			return fmt.Errorf("llm scene %q provider %q does not support %s", scene, resolved.Provider, capabilityName)
		}
		if visionScenes[scene] && !spec.Supports(llmprovider.CapabilityVision) {
			return fmt.Errorf("llm scene %q provider %q does not support vision input", scene, resolved.Provider)
		}
		if strings.TrimSpace(resolved.Model) == "" {
			return fmt.Errorf("llm scene %q has empty model", scene)
		}
		if spec.RequiresBaseURL && strings.TrimSpace(resolved.BaseURL) == "" {
			return fmt.Errorf("llm scene %q provider %q requires base_url", scene, resolved.Provider)
		}
		if resolved.TimeoutSeconds <= 0 {
			return fmt.Errorf("llm scene %q timeout_seconds must be > 0", scene)
		}
		return nil
	}
	if err := check(LLMSceneTaskPlanner); err != nil {
		return err
	}
	for scene := range c.LLM.Scenes {
		raw := c.LLM.Scenes[scene]
		if len(raw.Routes) > 0 && (scene == LLMSceneEmbedding || scene == LLMSceneADK) {
			return fmt.Errorf("llm scene %q does not support dynamic routes", scene)
		}
		if err := validatePolicy(fmt.Sprintf("llm scene %q", scene), raw); err != nil {
			return err
		}
		if err := check(scene); err != nil {
			return err
		}
		for index, route := range raw.Routes {
			if err := c.validateLLMRoute(scene, index, route); err != nil {
				return err
			}
		}
	}
	if err := c.validateLLMRouteCycles(); err != nil {
		return err
	}
	for scene := range c.LLM.Scenes {
		seen := map[string]bool{scene: true}
		current := scene
		for {
			fallback := c.ResolveLLMScene(current).FallbackScene
			if fallback == "" {
				break
			}
			if _, ok := c.LLM.Scenes[fallback]; !ok {
				return fmt.Errorf("llm scene %q references unknown fallback_scene %q", current, fallback)
			}
			if seen[fallback] {
				return fmt.Errorf("llm fallback cycle detected at scene %q", fallback)
			}
			seen[fallback] = true
			current = fallback
		}
	}
	if _, ok := c.LLM.Scenes[LLMSceneADK]; ok {
		provider := c.ResolveLLMScene(LLMSceneADK).Provider
		spec, _ := llmprovider.Lookup(provider)
		if spec.Protocol != llmprovider.ProtocolGemini {
			return fmt.Errorf("llm scene %q only supports gemini provider, got %q", LLMSceneADK, provider)
		}
	}
	return nil
}

func knownAnswerPipelineStage(stage string) bool {
	switch strings.TrimSpace(stage) {
	case "citation_verify", "fact_freshness_check", "numeric_consistency_check", "answer_uncertainty_calibrate", "safety_guard_output":
		return true
	default:
		return false
	}
}

func (c *Config) validateLLMRoute(scene string, index int, route LLMRouteRule) error {
	name := fmt.Sprintf("llm scene %q route %d", scene, index)
	if strings.TrimSpace(route.TargetScene) == "" {
		return fmt.Errorf("%s target_scene is required", name)
	}
	if route.TargetScene != LLMSceneTaskPlanner {
		if _, exists := c.LLM.Scenes[route.TargetScene]; !exists {
			return fmt.Errorf("%s references unknown target_scene %q", name, route.TargetScene)
		}
	}
	if route.MinRemainingTokens == nil && route.MaxRemainingTokens == nil && route.MinStepCount == nil && route.MaxStepCount == nil && len(route.Intents) == 0 && len(route.Complexities) == 0 && len(route.CostTiers) == 0 && len(route.LatencyTiers) == 0 && len(route.QualityTiers) == 0 {
		return fmt.Errorf("%s must define at least one routing condition", name)
	}
	for field, values := range map[string][]string{
		"intents":       route.Intents,
		"complexities":  route.Complexities,
		"cost_tiers":    route.CostTiers,
		"latency_tiers": route.LatencyTiers,
		"quality_tiers": route.QualityTiers,
	} {
		if err := validateRouteValues(name, field, values); err != nil {
			return err
		}
	}
	for field, value := range map[string]*int{
		"min_remaining_tokens": route.MinRemainingTokens,
		"max_remaining_tokens": route.MaxRemainingTokens,
		"min_step_count":       route.MinStepCount,
		"max_step_count":       route.MaxStepCount,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s %s must be >= 0", name, field)
		}
	}
	if route.MinRemainingTokens != nil && route.MaxRemainingTokens != nil && *route.MinRemainingTokens > *route.MaxRemainingTokens {
		return fmt.Errorf("%s remaining token range is invalid", name)
	}
	if route.MinStepCount != nil && route.MaxStepCount != nil && *route.MinStepCount > *route.MaxStepCount {
		return fmt.Errorf("%s step range is invalid", name)
	}
	return nil
}

func validateRouteValues(routeName, field string, values []string) error {
	allowed := map[string]map[string]bool{
		"intents":       {"coding": true, "research": true, "writing": true, "data_analysis": true, "automation": true, "general": true},
		"complexities":  {"low": true, "medium": true, "high": true},
		"cost_tiers":    {"economy": true, "balanced": true, "unconstrained": true},
		"latency_tiers": {"fast": true, "balanced": true, "flexible": true},
		"quality_tiers": {"economy": true, "balanced": true, "quality": true},
	}[field]
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !allowed[value] {
			return fmt.Errorf("%s %s contains unsupported value %q", routeName, field, value)
		}
		if seen[value] {
			return fmt.Errorf("%s %s contains duplicate value %q", routeName, field, value)
		}
		seen[value] = true
	}
	return nil
}

func (c *Config) validateLLMRouteCycles() error {
	state := make(map[string]uint8)
	var visit func(string) error
	visit = func(scene string) error {
		if state[scene] == 1 {
			return fmt.Errorf("llm route cycle detected at scene %q", scene)
		}
		if state[scene] == 2 {
			return nil
		}
		state[scene] = 1
		for _, route := range c.LLM.Scenes[scene].Routes {
			if _, configured := c.LLM.Scenes[route.TargetScene]; configured {
				if err := visit(route.TargetScene); err != nil {
					return err
				}
			}
		}
		state[scene] = 2
		return nil
	}
	for scene := range c.LLM.Scenes {
		if err := visit(scene); err != nil {
			return err
		}
	}
	return nil
}
