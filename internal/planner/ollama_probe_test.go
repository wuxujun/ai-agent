package planner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func withProbeTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
}

func probeResponse(req *http.Request, models string) *http.Response {
	body := ""
	if req.URL.Path == "/api/tags" {
		body = `{"models":` + models + `}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestProbeOllama_Success(t *testing.T) {
	withProbeTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		models := `[{"name":"llama3:latest"},{"name":"nomic-embed-text:latest"}]`
		if req.URL.Host == "no-tag.test" {
			models = `[{"name":"llama3"}]`
		}
		return probeResponse(req, models), nil
	}))
	if err := ProbeOllama(context.Background(), "http://ollama.test", "llama3:latest"); err != nil {
		t.Fatal(err)
	}
	if err := ProbeOllama(context.Background(), "http://ollama.test", "llama3"); err != nil {
		t.Fatal(err)
	}
	if err := ProbeOllama(context.Background(), "http://no-tag.test", "llama3:latest"); err != nil {
		t.Fatal(err)
	}
}

func TestProbeOllama_ModelNotPulled(t *testing.T) {
	withProbeTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return probeResponse(req, `[{"name":"llama3:latest"}]`), nil
	}))
	err := ProbeOllama(context.Background(), "http://ollama.test", "nomic-embed-text")
	if err == nil || !strings.Contains(err.Error(), "has not been pulled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProbeOllama_ServiceNotRunning(t *testing.T) {
	withProbeTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))
	err := ProbeOllama(context.Background(), "http://ollama.test", "llama3")
	if err == nil || !strings.Contains(err.Error(), "not running or unreachable") {
		t.Fatalf("unexpected error: %v", err)
	}
}
