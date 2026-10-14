package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func recoveryAuth(t *testing.T) {
	t.Helper()
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) {
		c.API.APIKey = ""
		c.API.Auth.Mode = "api_key"
		c.API.Tenants = map[string]config.APITenantConfig{"a": {APIKey: "recovery-a"}, "b": {APIKey: "recovery-b"}}
	}))
}
func recoveryServer(t *testing.T, st store.Store, bus *EventBus) *httptest.Server {
	t.Helper()
	h := &Handler{store: st}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AccessLogMiddleware(), RecoveryMiddleware(), ErrorMiddleware(), AuthMiddleware())
	group := r.Group("/api/tasks")
	group.Use(TaskTenantMiddleware(st))
	group.GET("/:id/stream", func(c *gin.Context) { h.streamTaskWithBus(c, bus, 20*time.Millisecond) })
	group.GET("/:id", h.getTask)
	group.GET("/:id/trace", h.listTaskTrace)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return server
}
func recoveryRequest(t *testing.T, url, key, cursor string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", key)
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}
func recoveryEvent(t *testing.T, scanner *bufio.Scanner) StepEvent {
	t.Helper()
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id:") {
			t.Fatal("stream falsely advertised a replay cursor")
		}
		if strings.HasPrefix(line, "data: ") {
			var e StepEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			return e
		}
	}
	t.Fatalf("missing stream event: %v", scanner.Err())
	return StepEvent{}
}
func TestSSERecoveryAcrossHandlers(t *testing.T) {
	recoveryAuth(t)
	path := filepath.Join(t.TempDir(), "recovery.db")
	first, err := store.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	peer, err := store.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	busA, busB := newTestBus(time.Now), newTestBus(time.Now)
	nodeA, nodeB := recoveryServer(t, first, busA), recoveryServer(t, peer, busB)
	task := &types.Task{ID: "recovery-task", TenantID: "a", Status: types.StatusRunning, StepCount: 1, Trace: []types.StepTrace{{Step: 1, Action: "before-disconnect"}}}
	if err := first.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	stream := recoveryRequest(t, nodeA.URL+"/api/tasks/recovery-task/stream", "recovery-a", "")
	if stream.Header.Get("X-SSE-Recovery") != "snapshot" {
		t.Fatal("recovery mode missing")
	}
	initial := recoveryEvent(t, bufio.NewScanner(stream.Body))
	if initial.Snapshot == nil || initial.Snapshot.TraceCount != 1 || initial.Status != types.StatusRunning {
		t.Fatalf("initial durable snapshot: %+v", initial)
	}
	stream.Body.Close() // Forced disconnect; observing a task must not cancel it.
	task.Trace = append(task.Trace, types.StepTrace{Step: 2, Action: "during-disconnect"})
	task.StepCount = 2
	task.Status = types.StatusAwaitingApproval
	if err := first.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	reconnect := recoveryRequest(t, nodeB.URL+"/api/tasks/recovery-task/stream", "recovery-a", "an-old-event-id")
	scanner := bufio.NewScanner(reconnect.Body)
	got := recoveryEvent(t, scanner)
	if got.Status != types.StatusAwaitingApproval || got.Snapshot == nil || got.Snapshot.TraceCount != 2 || got.Snapshot.StepCount != 2 {
		t.Fatalf("peer did not reconcile nonterminal state: %+v", got)
	}
	task.Status = types.StatusPaused
	task.Trace = append(task.Trace, types.StepTrace{Step: 3, Action: "on-other-node"})
	task.StepCount = 3
	if err := first.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	// No publication on peer bus: nonterminal state must still be reconciled.
	for {
		got = recoveryEvent(t, scanner)
		if got.Status == types.StatusPaused {
			break
		}
	}
	if got.Snapshot == nil || got.Snapshot.TraceCount != 3 {
		t.Fatal("periodic durable trace count missing")
	}
	task.Status = types.StatusPartial
	task.FinalAnswer = "durable recovered answer"
	task.ErrorCode = "partial-result"
	if err := first.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	busA.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusPartial, Final: "local hint"})
	for {
		got = recoveryEvent(t, scanner)
		if got.isTerminal() {
			break
		}
	}
	if got.Final != task.FinalAnswer || got.ErrorCode != task.ErrorCode || got.Snapshot == nil {
		t.Fatalf("authoritative terminal state lost: %+v", got)
	}
	for scanner.Scan() {
	}
	if scanner.Err() != nil {
		t.Fatal(scanner.Err())
	}
	trace := recoveryRequest(t, nodeB.URL+"/api/tasks/recovery-task/trace?limit=100", "recovery-a", "")
	body, err := io.ReadAll(trace.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "during-disconnect") || !strings.Contains(string(body), "on-other-node") {
		t.Fatal("durable Trace recovery lost an action")
	}
	for _, key := range []string{"recovery-b", "invalid"} {
		denied := recoveryRequest(t, nodeB.URL+"/api/tasks/recovery-task/stream", key, "an-old-event-id")
		if denied.StatusCode != 404 && denied.StatusCode != 401 {
			t.Fatalf("reconnect bypassed authorization: %d", denied.StatusCode)
		}
	}
}

type failingRecoveryStore struct {
	store.Store
	unavailable atomic.Bool
}

func (s *failingRecoveryStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	if s.unavailable.Load() {
		return nil, errors.New("private backend error")
	}
	return s.Store.GetTask(ctx, id)
}
func TestSSERecoveryDoesNotTrustStaleTerminalHint(t *testing.T) {
	recoveryAuth(t)
	base := store.NewMemoryStore()
	st := &failingRecoveryStore{Store: base}
	bus := newTestBus(time.Now)
	task := &types.Task{ID: "stale-terminal", TenantID: "a", Status: types.StatusRunning}
	if err := base.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	bus.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusCompleted, Final: "obsolete cached result"})
	server := recoveryServer(t, st, bus)
	response := recoveryRequest(t, server.URL+"/api/tasks/stale-terminal/stream", "recovery-a", "")
	scanner := bufio.NewScanner(response.Body)
	if got := recoveryEvent(t, scanner); got.Status != types.StatusRunning || got.Final != "" {
		t.Fatal("stale sticky cache replaced persisted state")
	}
	st.unavailable.Store(true)
	bus.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusCompleted, Final: "unconfirmed result"})
	// Confirm the store failure is observed without exposing upstream details.
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "private backend error") || strings.Contains(line, "unconfirmed result") {
			t.Fatal("unconfirmed terminal/error exposed")
		}
		if line == ": reconciliation-unavailable" {
			break
		}
	}
	task.Status = types.StatusCompleted
	task.FinalAnswer = "confirmed result"
	if err := base.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	st.unavailable.Store(false)
	for {
		got := recoveryEvent(t, scanner)
		if got.isTerminal() {
			if got.Final != task.FinalAnswer {
				t.Fatal("recovered wrong final state")
			}
			break
		}
	}
}

type blockedStreamWriter struct {
	*httptest.ResponseRecorder
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedStreamWriter) Write(b []byte) (int, error) {
	if strings.Contains(string(b), `"token":"block"`) {
		w.once.Do(func() { close(w.blocked); <-w.release })
	}
	return w.ResponseRecorder.Write(b)
}
func TestSSERecoverySlowConsumerDroppedTerminal(t *testing.T) {
	st := store.NewMemoryStore()
	bus := newTestBus(time.Now)
	task := &types.Task{ID: "slow-recovery", Status: types.StatusRunning}
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	writer := &blockedStreamWriter{ResponseRecorder: httptest.NewRecorder(), blocked: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	c.Params = gin.Params{{Key: "id", Value: task.ID}}
	finished := make(chan struct{})
	go func() { defer close(finished); (&Handler{store: st}).streamTaskWithBus(c, bus, 20*time.Millisecond) }()
	// Wait for subscription before publishing the frame that blocks this writer.
	for i := 0; i < 100; i++ {
		bus.mu.RLock()
		n := len(bus.subs[task.ID])
		bus.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	bus.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusRunning, Token: "block"})
	select {
	case <-writer.blocked:
	case <-ctx.Done():
		close(writer.release)
		<-finished
		t.Fatal("writer never blocked")
	}
	for i := 0; i < 32; i++ {
		bus.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusRunning})
	}
	task.Status = types.StatusFailed
	task.ErrorCode = "recovered-failure"
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		close(writer.release)
		t.Fatal(err)
	}
	bus.Publish(task.ID, StepEvent{TaskID: task.ID, Status: types.StatusFailed, ErrorCode: "dropped-hint"})
	close(writer.release)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("dropped terminal event left stream open")
	}
	body := writer.Body.String()
	if !strings.Contains(body, `"error_code":"recovered-failure"`) || strings.Contains(body, "dropped-hint") {
		t.Fatal("slow consumer did not recover the persisted final state")
	}
	bus.mu.RLock()
	remaining := len(bus.subs[task.ID])
	bus.mu.RUnlock()
	if remaining != 0 {
		t.Fatal("stream subscriber leaked")
	}
}

type transitionDuringReadStore struct {
	store.Store
	bus  *EventBus
	once sync.Once
}

func (s *transitionDuringReadStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	before, err := s.Store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	s.once.Do(func() {
		updated := *before
		updated.Status = types.StatusCompleted
		updated.FinalAnswer = "transition during initial read"
		err = s.Store.SaveFullTask(ctx, &updated)
		if err == nil {
			s.bus.Publish(id, StepEvent{TaskID: id, Status: types.StatusCompleted})
		}
	})
	return before, err
}
func TestSSERecoveryInitialReadTransitionHasNoSubscriptionGap(t *testing.T) {
	base := store.NewMemoryStore()
	bus := newTestBus(time.Now)
	if err := base.CreateTask(t.Context(), &types.Task{ID: "read-transition", Status: types.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	st := &transitionDuringReadStore{Store: base, bus: bus}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	c.Params = gin.Params{{Key: "id", Value: "read-transition"}}
	// Disable the periodic backstop so only the live subscription closes this gap.
	(&Handler{store: st}).streamTaskWithBus(c, bus, time.Hour)
	if ctx.Err() != nil || !strings.Contains(writer.Body.String(), "transition during initial read") {
		t.Fatal("initial read/subscription race lost the terminal transition")
	}
}

func TestSSERecoveryInitialStoreFailureIsNotMissingTask(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		st := &failingRecoveryStore{Store: store.NewMemoryStore()}
		st.unavailable.Store(unavailable)
		bus := newTestBus(time.Now)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Params = gin.Params{{Key: "id", Value: "missing"}}
		(&Handler{store: st}).streamTaskWithBus(c, bus, time.Second)
		want := http.StatusNotFound
		if unavailable {
			want = http.StatusServiceUnavailable
		}
		if w.Code != want || strings.Contains(w.Body.String(), "private backend error") {
			t.Fatalf("initial error contract: code=%d", w.Code)
		}
		if len(bus.subs) != 0 {
			t.Fatal("initial failure leaked a subscriber")
		}
	}
}
