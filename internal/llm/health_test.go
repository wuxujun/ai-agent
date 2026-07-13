package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

func TestLiteLLMHealthURL(t *testing.T) {
	got, err := liteLLMHealthURL("http://litellm:4000/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://litellm:4000/health/liveliness" {
		t.Fatalf("health URL = %q", got)
	}
}

func TestLiteLLMModelsURL(t *testing.T) {
	got, err := liteLLMModelsURL("http://litellm:4000/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://litellm:4000/v1/models" {
		t.Fatalf("models URL = %q", got)
	}
}

func TestProbeLiteLLMListsAvailableModels(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			return testHTTPResponse(http.StatusUnauthorized, "secret response must not escape"), nil
		}
		switch r.URL.Path {
		case "/health/liveliness":
			return testHTTPResponse(http.StatusOK, ""), nil
		case "/v1/models":
			return testHTTPResponse(http.StatusOK, `{"data":[{"id":"agent-writer"},{"id":"agent-planner"}]}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}

	probe := probeLiteLLMWithClient(context.Background(), client, "http://litellm/health/liveliness", "http://litellm/v1/models", "test-key")
	if probe.err != nil {
		t.Fatal(probe.err)
	}
	if !probe.gatewayReachable {
		t.Fatal("gateway should be reachable")
	}
	if _, ok := probe.models["agent-writer"]; !ok {
		t.Fatalf("models = %#v, want agent-writer", probe.models)
	}
}

func TestProbeLiteLLMDoesNotExposeErrorBody(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusUnauthorized, "secret response must not escape"), nil
	})}

	probe := probeLiteLLMWithClient(context.Background(), client, "http://litellm/health", "http://litellm/models", "bad-key")
	if probe.err == nil || probe.err.Error() != "gateway health: status 401" {
		t.Fatalf("error = %v", probe.err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHealthConfigKeyChangesWithSceneEndpoint(t *testing.T) {
	first := &config.Config{}
	first.LLM.Provider = "openai"
	first.LLM.Scenes = map[string]config.LLMEndpointConfig{"writer": {Provider: "litellm", Model: "writer", BaseURL: "http://one/v1/chat/completions"}}
	second := *first
	second.LLM.Scenes = map[string]config.LLMEndpointConfig{"writer": {Provider: "litellm", Model: "writer", BaseURL: "http://two/v1/chat/completions"}}
	if healthConfigKey(first) == healthConfigKey(&second) {
		t.Fatal("health cache key did not change with scene endpoint")
	}
}

func TestHealthConfigKeyChangesWithModelOrCredential(t *testing.T) {
	first := &config.Config{}
	first.LLM.Provider = "litellm"
	first.LLM.Model = "writer-v1"
	first.LLM.BaseURL = "http://litellm/v1/chat/completions"
	first.LLM.APIKey = "key-one"

	changedModel := *first
	changedModel.LLM.Model = "writer-v2"
	if healthConfigKey(first) == healthConfigKey(&changedModel) {
		t.Fatal("health cache key did not change with model")
	}

	changedCredential := *first
	changedCredential.LLM.APIKey = "key-two"
	if healthConfigKey(first) == healthConfigKey(&changedCredential) {
		t.Fatal("health cache key did not change with credential")
	}
	if strings.Contains(healthConfigKey(first), "key-one") {
		t.Fatal("health cache key contains plaintext credential")
	}
}
