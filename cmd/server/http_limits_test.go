package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPReadLimitsPreserveLongStreamingResponses(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, "data: last\n\n")
	})
	cfg := newHTTPServer("", handler)
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("unsafe HTTP defaults: %+v", cfg)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config = cfg
	server.Config.ReadHeaderTimeout = 1250 * time.Millisecond
	server.Config.ReadTimeout = 250 * time.Millisecond
	server.Start()
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !strings.Contains(string(body), "data: last") {
		t.Fatalf("read timeout broke SSE: %q %v", body, err)
	}
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost:"); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(conn)
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("incomplete header connection was not closed")
	}
}
