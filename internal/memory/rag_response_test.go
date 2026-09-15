package memory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The transport double exposes how many bytes the real receiving code reads,
// including when Content-Length is absent or misleading.
type ragCountingBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *ragCountingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *ragCountingBody) Close() error {
	b.closed = true
	return nil
}

func withRAGTransport(t *testing.T, transport roundTripFunc) {
	t.Helper()
	original := ragHTTPClient
	ragHTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() { ragHTTPClient = original })
}

func callRAGReceiver(ctx context.Context, receiver string) error {
	if receiver == "http" {
		_, err := doRAGRequest(ctx, "https://rag.test/search", "GET", "test", "", false)
		return err
	}
	var target any
	var dest any = &target
	if receiver == "notification" {
		dest = nil
	}
	_, err := sendJSONRPCWithHeaders(ctx, "https://rag.test/mcp", "", map[string]any{"method": "test"}, dest, nil)
	return err
}

// Removing the read limit would accept a valid JSON response over 4 MiB and
// consume its entire body, even when no decoded response was requested.
func TestRAGResponseByteLimit(t *testing.T) {
	for _, receiver := range []string{"http", "jsonrpc", "notification"} {
		for _, size := range []struct {
			name string
			n    int
		}{
			{"below", (4 << 20) - 1},
			{"exact", 4 << 20},
			{"one_over", (4 << 20) + 1},
			{"large", 8 << 20},
		} {
			t.Run(receiver+"/"+size.name, func(t *testing.T) {
				const value = `{"results":[{"id":"within-budget"}]}`
				body := &ragCountingBody{Reader: strings.NewReader(value + strings.Repeat(" ", size.n-len(value)))}
				withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
					length := int64(-1)
					if size.name == "large" {
						length = 1 // A transport can supply a misleading length.
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: length, Request: req}, nil
				})
				err := callRAGReceiver(context.Background(), receiver)
				if size.n > 4<<20 {
					if err == nil || !strings.Contains(err.Error(), "limit") {
						t.Errorf("expected explicit byte-limit rejection, got %v", err)
					}
				} else if err != nil {
					t.Errorf("response within limit rejected: %v", err)
				}
				if body.read > (4<<20)+1 {
					t.Errorf("read %d bytes, want at most 4 MiB plus one overflow probe", body.read)
				}
				if !body.closed {
					t.Error("response body not closed")
				}
			})
		}
	}
}

// An upstream error must not amplify memory use or expose its raw content in
// the returned error. Protocol fallback still has its existing end-to-end test.
func TestRAGErrorResponseByteLimit(t *testing.T) {
	for _, receiver := range []string{"http", "jsonrpc", "notification"} {
		t.Run(receiver, func(t *testing.T) {
			body := &ragCountingBody{Reader: strings.NewReader("private-upstream-content " + strings.Repeat("x", 64<<10))}
			withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: body, ContentLength: -1, Request: req}, nil
			})
			err := callRAGReceiver(context.Background(), receiver)
			if err == nil {
				t.Fatal("expected HTTP status error")
			}
			if !strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "private-upstream-content") || len(err.Error()) > 200 {
				t.Error("status error must retain status without exposing upstream body")
			}
			if body.read > 4<<10 {
				t.Errorf("read %d error bytes, want at most 4 KiB", body.read)
			}
			if !body.closed {
				t.Error("error response body not closed")
			}
		})
	}
}

// Overlong lines and many short comments must both stop the handshake, not
// bypass its budget by falling back to another request.
func TestMCPSSEByteLimits(t *testing.T) {
	const endpoint = "event: endpoint\ndata: /message\n\n"
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"line_exact", ":" + strings.Repeat(" ", (64<<10)-2) + "\n" + endpoint, false},
		{"line_over", ":" + strings.Repeat(" ", (64<<10)-1) + "\n" + endpoint, true},
		{"line_without_newline", strings.Repeat(":", 128<<10), true},
		{"total_exact", strings.Repeat(strings.Repeat(":", 4095)+"\n", 1023) + strings.Repeat("\n", 4096-len(endpoint)) + endpoint, false},
		{"total_over", strings.Repeat(strings.Repeat(":", 4095)+"\n", 1024) + ":\n" + endpoint, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &ragCountingBody{Reader: strings.NewReader(tc.body)}
			posts := 0
			withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}, nil
				}
				posts++
				if body.closed {
					t.Error("successful SSE session closed before initialization/tool call")
				}
				if req.URL.Path != "/message" {
					t.Errorf("POST path = %q, want resolved SSE endpoint", req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"content":[]}}`)), Request: req}, nil
			})
			_, err := queryMCP(context.Background(), "https://rag.test/events", "", "query")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "limit") {
					t.Errorf("expected explicit handshake limit error, got %v", err)
				}
				if posts != 0 {
					t.Errorf("budget violation triggered %d POST requests", posts)
				}
			} else if err != nil || posts != 3 {
				t.Errorf("valid SSE initialization failed: err=%v, posts=%d", err, posts)
			}
			if body.read > (4<<20)+1 {
				t.Errorf("read %d SSE bytes, want at most 4 MiB plus one probe", body.read)
			}
			if tc.name == "line_without_newline" && body.read > (64<<10)+1 {
				t.Errorf("read %d unterminated line bytes, want at most 64 KiB plus one probe", body.read)
			}
			if !body.closed {
				t.Error("SSE body not closed after query")
			}
		})
	}
}

// This body behaves like a stalled network read: Close unblocks Read. Channels
// expose lifecycle ordering without sleeps or goroutine-count heuristics.
type ragBlockingBody struct {
	reader     *io.PipeReader
	started    chan struct{}
	returned   chan struct{}
	closed     chan struct{}
	startOnce  sync.Once
	returnOnce sync.Once
	closeOnce  sync.Once
}

func newRAGBlockingBody(t *testing.T) *ragBlockingBody {
	t.Helper()
	r, w := io.Pipe()
	body := &ragBlockingBody{reader: r, started: make(chan struct{}), returned: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = body.Close(); _ = w.Close() })
	return body
}

func (b *ragBlockingBody) Read(p []byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	n, err := b.reader.Read(p)
	b.returnOnce.Do(func() { close(b.returned) })
	return n, err
}

func (b *ragBlockingBody) Close() error {
	b.closeOnce.Do(func() {
		_ = b.reader.Close()
		close(b.closed)
	})
	return nil
}

func TestMCPSSECancellationDoesNotFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := newRAGBlockingBody(t)
	posts := 0
	withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts++
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}, nil
	})
	go func() {
		<-body.started
		cancel()
	}()
	_, err := queryMCP(ctx, "https://rag.test/events", "", "query")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context cancellation, got %v", err)
	}
	if posts != 0 {
		t.Errorf("cancelled handshake triggered %d POST requests", posts)
	}
	select {
	case <-body.returned:
	default:
		t.Error("query returned before stalled SSE reader exited")
	}
}

func TestMCPSSETimeoutClosesReadBeforeFallback(t *testing.T) {
	synctest.Test(t, testMCPSSETimeoutClosesReadBeforeFallback)
}

func testMCPSSETimeoutClosesReadBeforeFallback(t *testing.T) {
	body := newRAGBlockingBody(t)
	posts := 0
	withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}, nil
		}
		posts++
		select {
		case <-body.closed:
		default:
			t.Error("fallback started before timed-out SSE body was closed")
		}
		select {
		case <-body.returned:
		default:
			t.Error("fallback started before timed-out SSE reader exited")
		}
		if req.Context().Err() != nil {
			t.Error("handshake timeout cancelled the parent POST context")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"content":[]}}`)), Request: req}, nil
	})
	_, err := queryMCP(context.Background(), "https://rag.test/events", "", "query")
	if err != nil || posts != 3 {
		t.Errorf("timeout fallback failed: err=%v, posts=%d", err, posts)
	}
}

func TestMCPSSEDiscoveryTimerStopsBeforeLongPOST(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &ragCountingBody{Reader: strings.NewReader("event: endpoint\ndata: /message\n\n")}
		posts := 0
		withRAGTransport(t, func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "Mcp-Session-Id": {"session-fixture"}}, Body: body, Request: req}, nil
			}
			posts++
			if posts == 1 {
				// Virtual time crosses the discovery deadline without slowing tests.
				time.Sleep(4 * time.Second)
				synctest.Wait()
			}
			if body.closed || req.Context().Err() != nil {
				t.Error("discovery timer interrupted a successfully established session")
			}
			if req.Header.Get("Mcp-Session-Id") != "session-fixture" {
				t.Error("SSE session header was not carried to POST")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"content":[]}}`)), Request: req}, nil
		})
		_, err := queryMCP(context.Background(), "https://rag.test/events", "", "query")
		if err != nil || posts != 3 || !body.closed {
			t.Errorf("long POST session lifecycle: err=%v, posts=%d, closed=%v", err, posts, body.closed)
		}
	})
}

type ragFillReader struct{}

func (ragFillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func BenchmarkRAGResponseRead(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"1KiB", 1 << 10},
		{"4MiB", 4 << 20},
		{"8MiB_rejected", 8 << 20},
		{"64MiB_rejected", 64 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				data, err := readRAGResponse(io.LimitReader(ragFillReader{}, tc.size))
				if tc.size > 4<<20 {
					if !errors.Is(err, errRAGResponseLimit) || data != nil {
						b.Fatalf("oversized response not rejected: err=%v", err)
					}
				} else if err != nil || int64(len(data)) != tc.size {
					b.Fatalf("bounded read failed: bytes=%d, err=%v", len(data), err)
				}
			}
		})
	}
}
