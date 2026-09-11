package memory

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/multiagent"
)

type embeddingBoundsTransport func(*http.Request) (*http.Response, error)

func (f embeddingBoundsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type embeddingCountedBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *embeddingCountedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *embeddingCountedBody) Close() error { b.closed = true; return nil }
func TestEmbeddingResponsesAreBoundedAndErrorsPrivate(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, provider := range []string{"openai", "ollama"} {
		for _, status := range []int{200, 503} {
			t.Run(provider+http.StatusText(status), func(t *testing.T) {
				payload := `{"data":[{"embedding":[1,2]}],"embedding":[1,2],"padding":"` + strings.Repeat("x", 5<<20) + `"}`
				limit := 4 << 20
				if status != 200 {
					payload = strings.Repeat("UPSTREAM_PRIVATE_MARKER", 1000)
					limit = 4 << 10
				}
				body := &embeddingCountedBody{Reader: strings.NewReader(payload)}
				http.DefaultTransport = embeddingBoundsTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})
				cfg := multiagent.LLMConfig{BaseURL: "https://fixture.invalid"}
				var err error
				if provider == "openai" {
					_, err = getOpenAIEmbedding(t.Context(), "fixture", cfg)
				} else {
					_, err = getOllamaEmbedding(t.Context(), "fixture", cfg)
				}
				if err == nil || strings.Contains(err.Error(), "UPSTREAM_PRIVATE_MARKER") || body.read > limit+1 || !body.closed {
					t.Fatalf("read=%d closed=%v err=%v", body.read, body.closed, err)
				}
				if status != 200 {
					var statusErr *llmcore.HTTPStatusError
					if !errors.As(err, &statusErr) || statusErr.StatusCode != status {
						t.Fatalf("status diagnostic lost: %v", err)
					}
				}
			})
		}
	}
}
