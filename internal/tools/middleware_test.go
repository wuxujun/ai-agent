package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/types"
)

type retryStubTool struct {
	name      string
	risk      types.RiskLevel
	policy    RetryPolicy
	failures  int
	callCount int
}

func (t *retryStubTool) Name() string        { return t.name }
func (t *retryStubTool) Description() string { return "retry stub" }
func (t *retryStubTool) Parameters() map[string]any {
	return map[string]any{"x": map[string]any{"type": "string"}}
}
func (t *retryStubTool) RiskLevel() types.RiskLevel { return t.risk }
func (t *retryStubTool) RetryPolicy() RetryPolicy   { return t.policy }
func (t *retryStubTool) Execute(context.Context, string, map[string]interface{}) (*ToolResult, error) {
	t.callCount++
	if t.callCount <= t.failures {
		return nil, fmt.Errorf("transient failure %d", t.callCount)
	}
	return &ToolResult{Observation: "ok"}, nil
}

func TestMiddlewareRetriesOnlyWhenPolicyAllows(t *testing.T) {
	tool := &retryStubTool{
		name:     "retryable_low_risk",
		risk:     types.RiskLevelLow,
		policy:   RetryPolicy{MaxRetries: 2, Backoff: time.Millisecond},
		failures: 2,
	}

	res, err := (&toolMiddleware{Tool: tool}).Execute(context.Background(), ".", map[string]interface{}{})
	if err != nil {
		t.Fatalf("expected retryable tool to eventually succeed: %v", err)
	}
	if res.Observation != "ok" {
		t.Fatalf("unexpected observation: %q", res.Observation)
	}
	if tool.callCount != 3 {
		t.Fatalf("expected 3 attempts, got %d", tool.callCount)
	}
}

func TestMiddlewareDoesNotRetryHighRiskTools(t *testing.T) {
	tool := &retryStubTool{
		name:     "high_risk_even_with_policy",
		risk:     types.RiskLevelHigh,
		policy:   RetryPolicy{MaxRetries: 2, Backoff: time.Millisecond},
		failures: 2,
	}

	_, err := (&toolMiddleware{Tool: tool}).Execute(context.Background(), ".", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected high-risk tool failure")
	}
	if tool.callCount != 1 {
		t.Fatalf("high-risk tool must not be retried, got %d attempts", tool.callCount)
	}
	if !strings.Contains(err.Error(), "failed after 1 attempts") {
		t.Fatalf("expected single-attempt failure, got %v", err)
	}
}

type noPolicyStubTool struct {
	callCount int
}

func (t *noPolicyStubTool) Name() string               { return "low_risk_without_policy" }
func (t *noPolicyStubTool) Description() string        { return "no policy stub" }
func (t *noPolicyStubTool) Parameters() map[string]any { return map[string]any{} }
func (t *noPolicyStubTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (t *noPolicyStubTool) Execute(context.Context, string, map[string]interface{}) (*ToolResult, error) {
	t.callCount++
	return nil, fmt.Errorf("permanent failure")
}

func TestMiddlewareDoesNotRetryToolsWithoutPolicy(t *testing.T) {
	tool := &noPolicyStubTool{}

	_, err := (&toolMiddleware{Tool: tool}).Execute(context.Background(), ".", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected tool failure")
	}
	if tool.callCount != 1 {
		t.Fatalf("tool without retry policy must not be retried, got %d attempts", tool.callCount)
	}
}

func TestMiddlewareTruncatesObservationAtUTF8Boundary(t *testing.T) {
	// 3999 ASCII bytes followed by U+00A2 makes byte 4000 equal 0xC2, exactly
	// the invalid prefix observed in PostgreSQL SQLSTATE 22021.
	value := strings.Repeat("a", 3999) + "¢" + strings.Repeat("b", 10)
	got := truncateToolObservation(value, 4000)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated observation is invalid UTF-8: %q", got[len(got)-40:])
	}
	if !strings.HasSuffix(got, "\n...[truncated by middleware]") {
		t.Fatalf("truncation marker missing: %q", got[len(got)-40:])
	}
	if strings.Contains(got, "¢") {
		t.Fatal("partially fitting multibyte rune should not be included")
	}
}

func TestMiddlewareNormalizesInvalidToolOutput(t *testing.T) {
	got := truncateToolObservation("before\xc2\nafter", 4000)
	if !utf8.ValidString(got) || !strings.Contains(got, "\uFFFD") {
		t.Fatalf("invalid tool output was not normalized: %q", got)
	}
}
