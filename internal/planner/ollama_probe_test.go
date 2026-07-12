package planner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeOllama_Success(t *testing.T) {
	// Mock server that returns a list of pulled models
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"models":[{"name":"llama3:latest"},{"name":"nomic-embed-text:latest"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx := context.Background()

	// 1. Match exact model name with latest tag
	if err := ProbeOllama(ctx, ts.URL, "llama3:latest"); err != nil {
		t.Errorf("expected success for llama3:latest, got: %v", err)
	}

	// 2. Match model name without tag (should default to matching with :latest tag)
	if err := ProbeOllama(ctx, ts.URL, "llama3"); err != nil {
		t.Errorf("expected success for llama3, got: %v", err)
	}

	// 3. Match model name with latest tag when returned name has no tag
	// (Simulate by returning no-tag model list)
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"models":[{"name":"llama3"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts2.Close()

	if err := ProbeOllama(ctx, ts2.URL, "llama3:latest"); err != nil {
		t.Errorf("expected success for llama3:latest with no-tag model on server, got: %v", err)
	}
}

func TestProbeOllama_ModelNotPulled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"models":[{"name":"llama3:latest"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx := context.Background()

	// Model not pulled should fail
	err := ProbeOllama(ctx, ts.URL, "nomic-embed-text")
	if err == nil {
		t.Fatal("expected ProbeOllama to fail for missing model, but got success")
	}
	if !strings.Contains(err.Error(), "has not been pulled") {
		t.Errorf("expected missing model message, got: %v", err)
	}
}

func TestProbeOllama_ServiceNotRunning(t *testing.T) {
	ctx := context.Background()

	// Use an invalid port to simulate service not running
	err := ProbeOllama(ctx, "http://localhost:9999", "llama3")
	if err == nil {
		t.Fatal("expected ProbeOllama to fail for dead port, but got success")
	}
	if !strings.Contains(err.Error(), "not running or unreachable") {
		t.Errorf("expected service unreachable message, got: %v", err)
	}
}
