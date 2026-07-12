package llm

import (
	"context"
	"errors"
	"testing"
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

func TestWaitRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitRetry(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitRetry error = %v", err)
	}
}
