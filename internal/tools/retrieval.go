package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

type retrievalContextKey struct{}

type retrievalExecutionContext struct {
	TaskID   string
	TenantID string
}

// WithRetrievalExecutionContext makes task identity available to JIT retrieval
// tools without expanding the generic Tool.Execute signature.
func WithRetrievalExecutionContext(ctx context.Context, taskID, tenantID string) context.Context {
	return context.WithValue(ctx, retrievalContextKey{}, retrievalExecutionContext{TaskID: taskID, TenantID: tenantID})
}

func retrievalExecutionFromContext(ctx context.Context) (retrievalExecutionContext, error) {
	value, ok := ctx.Value(retrievalContextKey{}).(retrievalExecutionContext)
	if !ok || strings.TrimSpace(value.TaskID) == "" {
		return retrievalExecutionContext{}, fmt.Errorf("retrieval tool requires task execution context")
	}
	return value, nil
}

type MemoryQueryStore interface {
	QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error)
}

type RetrievalDependencies struct {
	SearchRAG    func(context.Context, string) ([]types.Memory, error)
	GetEmbedding func(context.Context, string) ([]float32, error)
	MemoryStore  MemoryQueryStore
}

type retrievalCandidate struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Title     string       `json:"title,omitempty"`
	Snippet   string       `json:"snippet"`
	Score     float64      `json:"score,omitempty"`
	Source    string       `json:"source,omitempty"`
	Timestamp string       `json:"timestamp,omitempty"`
	Memory    types.Memory `json:"-"`
}

type retrievalTaskCache struct {
	Candidates map[string]retrievalCandidate
	Searches   map[string][]string
	Calls      map[string]int
	Inflight   map[string]*retrievalSearchFlight
	UpdatedAt  time.Time
}

type retrievalSearchFlight struct {
	done chan struct{}
	err  error
}

type retrievalCache struct {
	mu    sync.Mutex
	tasks map[string]*retrievalTaskCache
}

var defaultRetrievalCache = &retrievalCache{tasks: make(map[string]*retrievalTaskCache)}

func (c *retrievalCache) task(taskID string) *retrievalTaskCache {
	now := time.Now()
	for id, item := range c.tasks {
		if now.Sub(item.UpdatedAt) > 30*time.Minute {
			delete(c.tasks, id)
		}
	}
	item := c.tasks[taskID]
	if item == nil {
		item = &retrievalTaskCache{Candidates: make(map[string]retrievalCandidate), Searches: make(map[string][]string), Calls: make(map[string]int), Inflight: make(map[string]*retrievalSearchFlight)}
		c.tasks[taskID] = item
	}
	item.UpdatedAt = now
	return item
}

func (c *retrievalCache) cachedSearch(taskID, key string) ([]retrievalCandidate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.task(taskID)
	ids, ok := item.Searches[key]
	if !ok {
		return nil, false
	}
	result := make([]retrievalCandidate, 0, len(ids))
	for _, id := range ids {
		if candidate, exists := item.Candidates[id]; exists {
			result = append(result, candidate)
		}
	}
	return result, true
}

func (c *retrievalCache) beginSearch(taskID, key, kind string, limit int) ([]retrievalCandidate, bool, *retrievalSearchFlight, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.task(taskID)
	if ids, ok := item.Searches[key]; ok {
		result := make([]retrievalCandidate, 0, len(ids))
		for _, id := range ids {
			if candidate, exists := item.Candidates[id]; exists {
				result = append(result, candidate)
			}
		}
		return result, true, nil, false, nil
	}
	for previousKey, ids := range item.Searches {
		if retrievalKeysEquivalent(previousKey, key, kind) {
			result := make([]retrievalCandidate, 0, len(ids))
			for _, id := range ids {
				if candidate, exists := item.Candidates[id]; exists {
					result = append(result, candidate)
				}
			}
			item.Searches[key] = append([]string(nil), ids...)
			return result, true, nil, false, nil
		}
	}
	if flight := item.Inflight[key]; flight != nil {
		return nil, false, flight, false, nil
	}
	if item.Calls[kind] >= limit {
		return nil, false, nil, false, fmt.Errorf("%s search call limit reached (%d)", kind, limit)
	}
	item.Calls[kind]++
	flight := &retrievalSearchFlight{done: make(chan struct{})}
	item.Inflight[key] = flight
	return nil, false, flight, true, nil
}

func retrievalKeysEquivalent(left, right, kind string) bool {
	prefix := kind + ":"
	if !strings.HasPrefix(left, prefix) || !strings.HasPrefix(right, prefix) {
		return false
	}
	a := normalizeRetrievalQuery(strings.TrimPrefix(left, prefix))
	b := normalizeRetrievalQuery(strings.TrimPrefix(right, prefix))
	if a == "" || b == "" {
		return false
	}
	if a == b || (utf8RuneLen(a) >= 4 && utf8RuneLen(b) >= 4 && (strings.Contains(a, b) || strings.Contains(b, a))) {
		return true
	}
	return runeBigramJaccard(a, b) >= 0.65
}

func normalizeRetrievalQuery(value string) string {
	value = strings.ToLower(value)
	for _, filler := range []string{"有哪些", "有哪个人", "哪些人", "名单", "成员", "老师", "请问", "查询", "搜索", "find", "search", "list"} {
		value = strings.ReplaceAll(value, filler, "")
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return -1
	}, value)
}

func utf8RuneLen(value string) int { return len([]rune(value)) }

func runeBigramJaccard(left, right string) float64 {
	bigrams := func(value string) map[string]struct{} {
		runes := []rune(value)
		result := make(map[string]struct{})
		if len(runes) == 1 {
			result[string(runes)] = struct{}{}
		}
		for i := 0; i+1 < len(runes); i++ {
			result[string(runes[i:i+2])] = struct{}{}
		}
		return result
	}
	a, b := bigrams(left), bigrams(right)
	intersection := 0
	for item := range a {
		if _, ok := b[item]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func (c *retrievalCache) completeSearch(taskID, key string, flight *retrievalSearchFlight, candidates []retrievalCandidate, searchErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.task(taskID)
	if searchErr == nil {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			item.Candidates[candidate.ID] = candidate
			ids = append(ids, candidate.ID)
		}
		item.Searches[key] = ids
	}
	if item.Inflight[key] == flight {
		delete(item.Inflight, key)
	}
	flight.err = searchErr
	close(flight.done)
}

func (c *retrievalCache) fetch(taskID, kind string, ids []string) ([]retrievalCandidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.task(taskID)
	result := make([]retrievalCandidate, 0, len(ids))
	for _, id := range ids {
		candidate, ok := item.Candidates[id]
		if !ok || candidate.Kind != kind {
			return nil, fmt.Errorf("%s candidate %q not found; call %s_search first", kind, id, kind)
		}
		result = append(result, candidate)
	}
	return result, nil
}

func ClearRetrievalContext(taskID string) {
	defaultRetrievalCache.mu.Lock()
	delete(defaultRetrievalCache.tasks, taskID)
	defaultRetrievalCache.mu.Unlock()
}

type retrievalSearchTool struct {
	kind string
	deps RetrievalDependencies
}

func (t *retrievalSearchTool) Name() string { return t.kind + "_search" }
func (t *retrievalSearchTool) Description() string {
	if t.kind == "rag" {
		return "Search current third-party RAG knowledge and return compact candidate IDs; use rag_fetch to read selected evidence"
	}
	return "Search local long-term historical memories and return compact candidate IDs; use memory_get to read selected memories"
}
func (t *retrievalSearchTool) Parameters() map[string]any {
	return map[string]any{
		"query": map[string]any{"type": "string", "description": "Focused retrieval query"},
		"top_k": map[string]any{"type": "integer", "description": "Maximum candidates to return, from 1 to 5"},
	}
}
func (t *retrievalSearchTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (t *retrievalSearchTool) Validate(params map[string]any) error {
	if strings.TrimSpace(stringParameter(params, "query")) == "" {
		return fmt.Errorf("%s requires non-empty query", t.Name())
	}
	topK := intParameter(params, "top_k")
	if topK < 0 || topK > 5 {
		return fmt.Errorf("%s top_k must be between 1 and 5", t.Name())
	}
	return nil
}

func (t *retrievalSearchTool) Execute(ctx context.Context, _ string, params map[string]interface{}) (*ToolResult, error) {
	exec, err := retrievalExecutionFromContext(ctx)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(stringParameter(params, "query"))
	topK := intParameter(params, "top_k")
	if topK <= 0 || topK > 5 {
		topK = 5
	}
	key := t.kind + ":" + strings.ToLower(strings.Join(strings.Fields(query), " "))
	maxCalls := config.Get().RAG.JITSearchMaxCalls
	if maxCalls <= 0 {
		maxCalls = 2
	}
	candidates, cached, flight, leader, err := defaultRetrievalCache.beginSearch(exec.TaskID, key, t.kind, maxCalls)
	if err != nil {
		return nil, err
	}
	if cached {
		return searchCandidatesResult(query, candidates, true)
	}
	if !leader {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
		}
		if flight.err != nil {
			return nil, flight.err
		}
		candidates, ok := defaultRetrievalCache.cachedSearch(exec.TaskID, key)
		if !ok {
			return nil, fmt.Errorf("%s concurrent search completed without cached results", t.kind)
		}
		return searchCandidatesResult(query, candidates, true)
	}

	var memories []types.Memory
	if t.kind == "rag" {
		if t.deps.SearchRAG == nil {
			err = fmt.Errorf("rag_search is not configured")
			defaultRetrievalCache.completeSearch(exec.TaskID, key, flight, nil, err)
			return nil, err
		}
		memories, err = t.deps.SearchRAG(ctx, query)
	} else {
		if t.deps.MemoryStore == nil || t.deps.GetEmbedding == nil {
			err = fmt.Errorf("memory_search is not configured")
			defaultRetrievalCache.completeSearch(exec.TaskID, key, flight, nil, err)
			return nil, err
		}
		var embedding []float32
		embedding, err = t.deps.GetEmbedding(ctx, query)
		if err == nil {
			var stored []*types.Memory
			stored, err = t.deps.MemoryStore.QueryMemories(ctx, query, embedding, topK)
			for _, item := range stored {
				if item != nil {
					memories = append(memories, *item)
				}
			}
		}
	}
	if err != nil {
		defaultRetrievalCache.completeSearch(exec.TaskID, key, flight, nil, err)
		return nil, err
	}
	if len(memories) > topK {
		memories = memories[:topK]
	}
	candidates = make([]retrievalCandidate, 0, len(memories))
	queryHash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:10]
	for i, item := range memories {
		title := strings.TrimSpace(item.Goal)
		if title == "" {
			title = t.kind + " result " + strconv.Itoa(i+1)
		}
		snippetSource := item.KeyFindings
		if strings.TrimSpace(snippetSource) == "" {
			snippetSource = item.FinalAnswer
		}
		snippet, _ := truncateRetrievalBytes(strings.TrimSpace(snippetSource), 500)
		source := item.TaskID
		if t.kind == "rag" {
			source = "third_party_rag"
		}
		timestamp := ""
		if !item.Timestamp.IsZero() {
			timestamp = item.Timestamp.Format(time.RFC3339)
		}
		candidates = append(candidates, retrievalCandidate{ID: fmt.Sprintf("%s-%s-%d", t.kind, queryHash, i+1), Kind: t.kind, Title: title, Snippet: snippet, Source: source, Timestamp: timestamp, Memory: item})
	}
	defaultRetrievalCache.completeSearch(exec.TaskID, key, flight, candidates, nil)
	return searchCandidatesResult(query, candidates, false)
}

func searchCandidatesResult(query string, candidates []retrievalCandidate, cached bool) (*ToolResult, error) {
	public := make([]retrievalCandidate, len(candidates))
	copy(public, candidates)
	encoded, err := json.Marshal(map[string]any{"cached": cached, "count": len(public), "results": public})
	if err != nil {
		return nil, err
	}
	return &ToolResult{Query: query, Observation: string(encoded)}, nil
}

type retrievalFetchTool struct{ kind string }

func (t *retrievalFetchTool) Name() string {
	if t.kind == "memory" {
		return "memory_get"
	}
	return "rag_fetch"
}
func (t *retrievalFetchTool) Description() string {
	if t.kind == "memory" {
		return "Read selected historical memory candidates by IDs returned from memory_search"
	}
	return "Read selected current RAG evidence by IDs returned from rag_search"
}
func (t *retrievalFetchTool) Parameters() map[string]any {
	return map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Candidate IDs returned by the matching search tool"}}
}
func (t *retrievalFetchTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (t *retrievalFetchTool) Validate(params map[string]any) error {
	ids := stringSliceParameter(params, "ids")
	if len(ids) == 0 {
		return fmt.Errorf("%s requires at least one candidate ID", t.Name())
	}
	limit := config.Get().RAG.JITFetchMaxItems
	if limit <= 0 {
		limit = 3
	}
	if len(ids) > limit {
		return fmt.Errorf("%s accepts at most %d candidate IDs", t.Name(), limit)
	}
	return nil
}
func (t *retrievalFetchTool) Execute(ctx context.Context, _ string, params map[string]interface{}) (*ToolResult, error) {
	exec, err := retrievalExecutionFromContext(ctx)
	if err != nil {
		return nil, err
	}
	ids := stringSliceParameter(params, "ids")
	if err := t.Validate(params); err != nil {
		return nil, err
	}
	candidates, err := defaultRetrievalCache.fetch(exec.TaskID, t.kind, ids)
	if err != nil {
		return nil, err
	}
	totalBudget := config.Get().RAG.JITRAGFetchMaxBytes
	if totalBudget <= 0 {
		totalBudget = 6000
	}
	if t.kind == "memory" {
		totalBudget = config.Get().RAG.JITMemoryFetchMaxBytes
		if totalBudget <= 0 {
			totalBudget = 2000
		}
	}
	remaining := totalBudget
	evidence := make([]types.Evidence, 0, len(candidates))
	for _, candidate := range candidates {
		content := strings.TrimSpace(strings.Join([]string{candidate.Memory.KeyFindings, candidate.Memory.FinalAnswer}, "\n"))
		content, _ = truncateRetrievalBytes(content, remaining)
		if content == "" {
			continue
		}
		evidence = append(evidence, types.Evidence{Path: candidate.ID, Query: candidate.Title, Lines: []string{content}})
		remaining -= len(content)
		if remaining <= 0 {
			break
		}
	}
	return &ToolResult{Query: strings.Join(ids, ","), Observation: fmt.Sprintf("fetched %d %s item(s)", len(evidence), t.kind), Evidence: evidence}, nil
}

func RegisterRetrievalTools(deps RetrievalDependencies) {
	Register(&retrievalSearchTool{kind: "rag", deps: deps})
	Register(&retrievalSearchTool{kind: "memory", deps: deps})
	Register(&retrievalFetchTool{kind: "rag"})
	Register(&retrievalFetchTool{kind: "memory"})
}

func init() { RegisterRetrievalTools(RetrievalDependencies{}) }

func stringParameter(params map[string]interface{}, key string) string {
	value, _ := params[key].(string)
	return value
}

func intParameter(params map[string]interface{}, key string) int {
	switch value := params[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func stringSliceParameter(params map[string]interface{}, key string) []string {
	switch value := params[key].(type) {
	case []string:
		return value
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func truncateRetrievalBytes(value string, limit int) (string, bool) {
	const suffix = "\n[truncated]"
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	allowed := limit - len(suffix)
	if allowed < 0 {
		allowed = limit
	}
	cut := 0
	for index := range value {
		if index > allowed {
			break
		}
		cut = index
	}
	if limit < len(suffix) {
		return value[:cut], true
	}
	return value[:cut] + suffix, true
}
