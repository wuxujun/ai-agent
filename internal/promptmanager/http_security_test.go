package promptmanager

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type promptCountedBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *promptCountedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *promptCountedBody) Close() error { b.closed = true; return nil }
func TestPromptResponseIsBoundedBeforeJSONDecode(t *testing.T) {
	body := &promptCountedBody{Reader: strings.NewReader(`{"prompt":"` + strings.Repeat("x", 2<<20) + `"}`)}
	manager := &PromptManager{client: &http.Client{Transport: promptRoundTripFunc(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200, Body: body}, nil })}}
	_, err := manager.fetchFromLangfuse(t.Context(), "name", Selector{}, "https://fixture.test", "public", "secret")
	if err == nil || body.read > (1<<20)+1 || !body.closed {
		t.Fatalf("bounded read=%d closed=%v err=%v", body.read, body.closed, err)
	}
}
func TestPromptRedirectDoesNotForwardBasicAuth(t *testing.T) {
	var called atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); w.Write([]byte(`{"prompt":"leaked"}`)) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	manager := &PromptManager{client: source.Client()}
	_, err := manager.fetchFromLangfuse(t.Context(), "name", Selector{}, source.URL, "public", "secret")
	if err == nil || called.Load() != 0 {
		t.Fatalf("redirect target called=%d err=%v", called.Load(), err)
	}
}
