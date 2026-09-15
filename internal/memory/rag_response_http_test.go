package memory

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRAGHTTPTransferByteLimit(t *testing.T) {
	for _, encoding := range []string{"chunked", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if encoding == "gzip" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush() // No Content-Length; use chunked transfer.
				var dst io.Writer = w
				if encoding == "gzip" {
					compressed := gzip.NewWriter(w)
					defer compressed.Close()
					dst = compressed
				}
				_, _ = io.WriteString(dst, `{"results":[]}`)
				// Generated lazily; the test server need not allocate an 8 MiB body.
				_, _ = io.CopyN(dst, ragFillReader{}, 8<<20)
			}))
			defer server.Close()
			original := ragHTTPClient
			ragHTTPClient = server.Client()
			defer func() { ragHTTPClient = original }()
			for _, receiver := range []string{"http", "jsonrpc"} {
				t.Run(receiver, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					var err error
					if receiver == "http" {
						_, err = doRAGRequest(ctx, server.URL, "GET", "query", "", false)
					} else {
						var target any
						_, err = sendJSONRPCWithHeaders(ctx, server.URL, "", map[string]any{"method": "test"}, &target, nil)
					}
					if !errors.Is(err, errRAGResponseLimit) {
						t.Fatalf("%s %s response: expected byte-limit rejection, got %v", encoding, receiver, err)
					}
				})
			}
		})
	}
}

func TestRAGHTTPBodyCancellation(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		for _, receiver := range []string{"http", "jsonrpc"} {
			t.Run(http.StatusText(status)+"/"+receiver, func(t *testing.T) {
				started, disconnected := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "{")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(disconnected)
				}))
				defer server.Close()
				original := ragHTTPClient
				ragHTTPClient = server.Client()
				transport := ragHTTPClient.Transport
				ragHTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(req)
					if err == nil {
						resp.Body = &ragReadSignalBody{ReadCloser: resp.Body, started: started}
					}
					return resp, err
				})
				defer func() { ragHTTPClient = original }()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
				}()
				var err error
				if receiver == "http" {
					_, err = doRAGRequest(ctx, server.URL, "GET", "query", "", false)
				} else {
					_, err = sendJSONRPCWithHeaders(ctx, server.URL, "", map[string]any{"method": "test"}, nil, nil)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("expected cancellation to propagate, got %v", err)
				}
				select {
				case <-disconnected:
				case <-time.After(time.Second):
					t.Error("upstream did not observe cancellation")
				}
			})
		}
	}
}

func TestRAGResponseReadErrorDiscardsPartialBody(t *testing.T) {
	want := errors.New("fixture interrupted stream")
	data, err := readRAGResponse(io.MultiReader(strings.NewReader(`{"results":[]}`), ragErrorReader{err: want}))
	if !errors.Is(err, want) || data != nil {
		t.Fatalf("partial body should not be parsed: bytes=%d, err=%v", len(data), err)
	}
}

type ragErrorReader struct{ err error }

func (r ragErrorReader) Read([]byte) (int, error) { return 0, r.err }

type ragReadSignalBody struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *ragReadSignalBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}
