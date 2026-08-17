package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/config"
)

func TestValidateStartupTeamRouting(t *testing.T) {
	valid := &config.Config{}
	valid.MultiAgent.Team = "wiki"
	valid.API.Tenants = map[string]config.APITenantConfig{
		"tenant-a": {AllowedMultiAgentTeams: []string{"wiki", "wiki_graph"}},
	}
	if err := validateStartupTeamRouting(valid); err != nil {
		t.Fatalf("valid startup routing rejected: %v", err)
	}

	invalid := &config.Config{}
	invalid.MultiAgent.Team = "missing-startup-team"
	if err := validateStartupTeamRouting(invalid); err == nil || !strings.Contains(err.Error(), `default Team "missing-startup-team" is not configured`) {
		t.Fatalf("invalid startup routing error = %v", err)
	}
}

type fakeApprovalBusCloser struct {
	closed atomic.Bool
	order  *[]string
}

func (f *fakeApprovalBusCloser) Close() {
	f.closed.Store(true)
	if f.order != nil {
		*f.order = append(*f.order, "bus")
	}
}

type fakeRedisClientCloser struct {
	closed atomic.Bool
	err    error
	order  *[]string
}

func (f *fakeRedisClientCloser) Close() error {
	f.closed.Store(true)
	if f.order != nil {
		*f.order = append(*f.order, "redis")
	}
	return f.err
}

type fakeAppHTTPServer struct {
	started     chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	listenErr   error
	shutdownErr error
	order       *[]string
}

func newFakeAppHTTPServer(order *[]string) *fakeAppHTTPServer {
	return &fakeAppHTTPServer{
		started: make(chan struct{}),
		stop:    make(chan struct{}),
		order:   order,
	}
}

func (f *fakeAppHTTPServer) ListenAndServe() error {
	close(f.started)
	if f.listenErr != nil {
		return f.listenErr
	}
	<-f.stop
	return http.ErrServerClosed
}

func (f *fakeAppHTTPServer) Shutdown(context.Context) error {
	if f.order != nil {
		*f.order = append(*f.order, "http")
	}
	f.stopOnce.Do(func() { close(f.stop) })
	return f.shutdownErr
}

type fakeAppTaskManager struct {
	err   error
	order *[]string
}

func (f *fakeAppTaskManager) Shutdown(context.Context) error {
	if f.order != nil {
		*f.order = append(*f.order, "tasks")
	}
	return f.err
}

func TestApprovalBusLogInfoExcludesRedisCredentials(t *testing.T) {
	const dsn = "rediss://redis-user:super-secret@redis.internal:6380/7"
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("ParseURL() error = %v", err)
	}

	info := newApprovalBusLogInfo(opts)
	if info.Address != "redis.internal:6380" || info.DB != 7 || !info.TLS {
		t.Fatalf("newApprovalBusLogInfo() = %+v", info)
	}

	logged := fmt.Sprintf("%+v", info)
	for _, secret := range []string{"redis-user", "super-secret", dsn} {
		if strings.Contains(logged, secret) {
			t.Fatalf("safe Redis log metadata contains credential %q: %s", secret, logged)
		}
	}
}

func TestApprovalBusLogInfoHandlesNilOptions(t *testing.T) {
	if got := newApprovalBusLogInfo(nil); got != (approvalBusLogInfo{}) {
		t.Fatalf("newApprovalBusLogInfo(nil) = %+v", got)
	}
}

func TestApprovalBusRuntimeCloseCancelsAndJoinsResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bus := &fakeApprovalBusCloser{}
	clientErr := errors.New("client close failed")
	client := &fakeRedisClientCloser{err: clientErr}
	runtime := &approvalBusRuntime{cancel: cancel, bus: bus, client: client}

	workerDone := make(chan struct{})
	runtime.wg.Add(1)
	go func() {
		defer runtime.wg.Done()
		<-ctx.Done()
		close(workerDone)
	}()

	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	select {
	case err := <-closed:
		if !errors.Is(err, clientErr) {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish; a background worker was not joined")
	}

	select {
	case <-workerDone:
	default:
		t.Fatal("background worker did not observe runtime cancellation")
	}
	if !bus.closed.Load() || !client.closed.Load() {
		t.Fatalf("resources not closed: bus=%v client=%v", bus.closed.Load(), client.closed.Load())
	}
}

func TestNilApprovalBusRuntimeClose(t *testing.T) {
	var runtime *approvalBusRuntime
	if err := runtime.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
}

func TestRunAppReloadsAndShutsDownInOrder(t *testing.T) {
	var order []string
	server := newFakeAppHTTPServer(&order)
	tasks := &fakeAppTaskManager{order: &order}
	bus := &fakeApprovalBusCloser{order: &order}
	client := &fakeRedisClientCloser{order: &order}
	reloaded := make(chan struct{}, 1)
	signals := make(chan os.Signal, 2)
	done := make(chan error, 1)

	go func() {
		done <- runApp(appRuntime{
			server: server,
			tasks:  tasks,
			bus:    &approvalBusRuntime{bus: bus, client: client},
			reload: func() error {
				reloaded <- struct{}{}
				return nil
			},
			shutdownTimeout: time.Second,
		}, signals)
	}()

	<-server.started
	signals <- syscall.SIGHUP
	select {
	case <-reloaded:
	case <-time.After(time.Second):
		t.Fatal("SIGHUP did not reload configuration")
	}
	select {
	case err := <-done:
		t.Fatalf("runApp stopped after SIGHUP: %v", err)
	default:
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runApp() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runApp did not stop after SIGTERM")
	}
	if got, want := strings.Join(order, ","), "http,tasks,bus,redis"; got != want {
		t.Fatalf("shutdown order = %q, want %q", got, want)
	}
}

func TestRunAppReturnsListenerErrorAfterCleanup(t *testing.T) {
	listenErr := errors.New("listener failed")
	var order []string
	server := newFakeAppHTTPServer(&order)
	server.listenErr = listenErr
	tasks := &fakeAppTaskManager{order: &order}
	bus := &fakeApprovalBusCloser{order: &order}
	client := &fakeRedisClientCloser{order: &order}

	err := runApp(appRuntime{
		server:          server,
		tasks:           tasks,
		bus:             &approvalBusRuntime{bus: bus, client: client},
		shutdownTimeout: time.Second,
	}, make(chan os.Signal))
	if !errors.Is(err, listenErr) {
		t.Fatalf("runApp() error = %v, want listener error", err)
	}
	if got, want := strings.Join(order, ","), "http,tasks,bus,redis"; got != want {
		t.Fatalf("cleanup order = %q, want %q", got, want)
	}
}

func TestRunAppJoinsShutdownErrors(t *testing.T) {
	httpErr := errors.New("HTTP shutdown failed")
	taskErr := errors.New("task drain failed")
	redisErr := errors.New("Redis close failed")
	server := newFakeAppHTTPServer(nil)
	server.shutdownErr = httpErr
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGINT

	err := runApp(appRuntime{
		server:          server,
		tasks:           &fakeAppTaskManager{err: taskErr},
		bus:             &approvalBusRuntime{client: &fakeRedisClientCloser{err: redisErr}},
		shutdownTimeout: time.Second,
	}, signals)
	for _, want := range []error{httpErr, taskErr, redisErr} {
		if !errors.Is(err, want) {
			t.Fatalf("runApp() error = %v, missing %v", err, want)
		}
	}
}
