package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&HTTPStatusError{StatusCode: 400}, false},
		{&HTTPStatusError{StatusCode: 401}, false},
		{&HTTPStatusError{StatusCode: 429}, true},
		{&HTTPStatusError{StatusCode: 503}, true},
		{context.Canceled, false},
		{errors.New("network"), true},
	}
	for _, item := range tests {
		if got := IsRetryable(item.err); got != item.want {
			t.Errorf("IsRetryable(%v)=%t want %t", item.err, got, item.want)
		}
	}
}

func TestNewHTTPStatusErrorRedactsBodyAndParsesRetryAfter(t *testing.T) {
	header := make(http.Header)
	header.Set("Retry-After", "7")
	err := NewHTTPStatusError(http.StatusTooManyRequests, header, []byte(`{"error":{"message":"api key sk-secret failed","type":"rate_limit_error","code":"rate_limit"}}`))
	if err.Body != "type=rate_limit_error code=rate_limit" {
		t.Fatalf("safe body = %q", err.Body)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error leaked response message: %v", err)
	}
	if err.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %s", err.RetryAfter)
	}
}

func TestNewHTTPStatusErrorOmitsUnstructuredBody(t *testing.T) {
	err := NewHTTPStatusError(http.StatusBadGateway, nil, []byte("upstream echoed secret"))
	if err.Body != "response body omitted" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	got := parseRetryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now)
	if got != 5*time.Second {
		t.Fatalf("retry delay = %s", got)
	}
}

func TestRetryDelayUsesRetryAfterWithCap(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 429, RetryAfter: time.Minute}
	if got := retryDelay(0, err); got != maxRetryAfter {
		t.Fatalf("retry delay = %s, want %s", got, maxRetryAfter)
	}
}

func TestRetryDelayUsesFullJitterBounds(t *testing.T) {
	for range 100 {
		got := retryDelay(2, errors.New("network"))
		if got < 0 || got > 400*time.Millisecond {
			t.Fatalf("retry delay outside jitter bounds: %s", got)
		}
	}
}

func TestWaitRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitRetry(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitRetry error = %v", err)
	}
}
