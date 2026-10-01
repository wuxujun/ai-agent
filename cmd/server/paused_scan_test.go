package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type blockingScanStore struct {
	store.Store
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	active    atomic.Bool
}

func (s *blockingScanStore) ListTasks(ctx context.Context, _ store.ListFilter) ([]*types.Task, error) {
	s.active.Store(true)
	defer s.active.Store(false)
	close(s.entered)
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	return nil, ctx.Err()
}

func TestPausedTaskScanShutdownJoinsBeforeStoreClose(t *testing.T) {
	for _, listenFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "signal", true: "listen failure"}[listenFailure], func(t *testing.T) {
			st := &blockingScanStore{Store: store.NewMemoryStore(), entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			runner := startPausedTaskScan(st, nil)
			select {
			case <-st.entered:
			case <-time.After(time.Second):
				t.Fatal("scan did not start")
			}
			server := newFakeAppHTTPServer(nil)
			if listenFailure {
				server.listenErr = errors.New("listen failed")
			}
			signals := make(chan os.Signal, 1)
			done := make(chan error, 1)
			go func() {
				err := runApp(appRuntime{server: server, tasks: &fakeAppTaskManager{}, paused: runner}, signals)
				if st.active.Load() {
					t.Error("store would close while scan active")
				}
				st.Close()
				done <- err
			}()
			<-server.started
			if !listenFailure {
				signals <- syscall.SIGTERM
			}
			select {
			case <-st.cancelled:
			case <-time.After(time.Second):
				t.Fatal("scan not cancelled")
			}
			select {
			case <-done:
				t.Fatal("shutdown did not join scan")
			case <-time.After(20 * time.Millisecond):
			}
			close(st.release)
			select {
			case err := <-done:
				if (err != nil) != listenFailure {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown did not finish")
			}
			runner.Stop()
			runner.Wait()
		})
	}
}
