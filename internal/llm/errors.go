package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxErrorBodyBytes = 4 << 10
	maxRetryAfter     = 30 * time.Second
)

type HTTPStatusError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("LLM API status %d: %s", e.StatusCode, e.Body)
}

func NewHTTPStatusError(statusCode int, header http.Header, raw []byte) *HTTPStatusError {
	return &HTTPStatusError{
		StatusCode: statusCode,
		Body:       safeErrorDetail(raw),
		RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now()),
	}
}

func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == 408 || statusErr.StatusCode == 429 || statusErr.StatusCode >= 500
	}
	return true
}

func WaitRetry(ctx context.Context, attempt int, retryErr ...error) error {
	var err error
	if len(retryErr) > 0 {
		err = retryErr[0]
	}
	delay := retryDelay(attempt, err)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(attempt int, err error) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > 0 {
		return min(statusErr.RetryAfter, maxRetryAfter)
	}
	ceiling := 100 * time.Millisecond * time.Duration(1<<min(attempt, 5))
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func safeErrorDetail(raw []byte) string {
	if len(raw) > maxErrorBodyBytes {
		raw = raw[:maxErrorBodyBytes]
	}
	var payload struct {
		Error struct {
			Type any `json:"type"`
			Code any `json:"code"`
		} `json:"error"`
		Type any `json:"type"`
		Code any `json:"code"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "response body omitted"
	}
	typeValue := safeMetadataValue(payload.Error.Type)
	if typeValue == "" {
		typeValue = safeMetadataValue(payload.Type)
	}
	codeValue := safeMetadataValue(payload.Error.Code)
	if codeValue == "" {
		codeValue = safeMetadataValue(payload.Code)
	}
	parts := make([]string, 0, 2)
	if typeValue != "" {
		parts = append(parts, "type="+typeValue)
	}
	if codeValue != "" {
		parts = append(parts, "code="+codeValue)
	}
	if len(parts) == 0 {
		return "response body omitted"
	}
	return strings.Join(parts, " ")
}

func safeMetadataValue(value any) string {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > 128 {
		return ""
	}
	for _, r := range text {
		if !(r == '_' || r == '-' || r == '.' || r == '/' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return ""
		}
	}
	return text
}
