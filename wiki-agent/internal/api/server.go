// Package api exposes the Wiki Agent over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/planner"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/wiki"
	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

const maxBodyBytes = 10 << 20

// Knowledge is the Wiki Agent surface used by the API.
type Knowledge interface {
	Ingest(ctx context.Context, req wiki.IngestRequest) (wiki.IngestResult, error)
	Retrieve(ctx context.Context, query string, opts models.SearchOptions) ([]models.RetrievedChunk, error)
}

// Planner produces plans for tasks.
type Planner interface {
	Plan(ctx context.Context, req models.PlanRequest) (models.Plan, error)
}

// Pinger reports database readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// NewHandler builds the HTTP routes.
func NewHandler(k Knowledge, p Planner, db Pinger, log *slog.Logger) http.Handler {
	h := &handler{k: k, p: p, db: db, log: log.With("component", "api")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("POST /v1/knowledge", h.ingest)
	mux.HandleFunc("POST /v1/knowledge/search", h.search)
	mux.HandleFunc("POST /v1/plans", h.plan)
	return h.logRequests(mux)
}

type handler struct {
	k   Knowledge
	p   Planner
	db  Pinger
	log *slog.Logger
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type ingestRequest struct {
	Document   models.Document `json:"document"`
	Confidence float64         `json:"confidence"`
}

func (h *handler) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.k.Ingest(r.Context(), wiki.IngestRequest{Document: req.Document, Confidence: req.Confidence})
	switch {
	case errors.Is(err, wiki.ErrRejected):
		writeJSON(w, http.StatusUnprocessableEntity, res)
	case err != nil:
		h.log.ErrorContext(r.Context(), "ingest failed", "error", err)
		writeError(w, http.StatusInternalServerError, "ingest failed")
	default:
		writeJSON(w, http.StatusCreated, res)
	}
}

type searchRequest struct {
	Query string `json:"query"`
	models.SearchOptions
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	res, err := h.k.Retrieve(r.Context(), req.Query, req.SearchOptions)
	if err != nil {
		h.log.ErrorContext(r.Context(), "search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	if res == nil {
		res = []models.RetrievedChunk{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

func (h *handler) plan(w http.ResponseWriter, r *http.Request) {
	var task models.Task
	if !decode(w, r, &task) {
		return
	}
	if task.ID == "" || task.Goal == "" {
		writeError(w, http.StatusBadRequest, "id and goal are required")
		return
	}
	plan, err := h.p.Plan(r.Context(), models.PlanRequest{Task: task})
	switch {
	case errors.Is(err, planner.ErrNoVerifiedKnowledge), errors.Is(err, planner.ErrUnverifiedReference), errors.Is(err, planner.ErrEmptyPlan):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		h.log.ErrorContext(r.Context(), "plan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "plan failed")
	default:
		writeJSON(w, http.StatusOK, plan)
	}
}

func (h *handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		h.log.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
