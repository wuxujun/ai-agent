package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeAppAssessments struct {
	order *[]string
	err   error
}

func (f *fakeAppAssessments) StopAssessments() { *f.order = append(*f.order, "assessment-stop") }
func (f *fakeAppAssessments) WaitAssessments(context.Context) error {
	*f.order = append(*f.order, "assessment-wait")
	return f.err
}

func TestRunAppOwnsAssessmentShutdown(t *testing.T) {
	for _, listenerFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "signal", true: "listener error"}[listenerFails], func(t *testing.T) {
			var order []string
			server := newFakeAppHTTPServer(&order)
			listenerErr := errors.New("listener failed")
			signals := make(chan os.Signal, 1)
			if listenerFails {
				server.listenErr = listenerErr
			} else {
				signals <- syscall.SIGTERM
			}
			assessmentErr := errors.New("assessment wait failed")
			err := runApp(appRuntime{server: server, tasks: &fakeAppTaskManager{order: &order}, assessments: &fakeAppAssessments{order: &order, err: assessmentErr}, bus: &approvalBusRuntime{bus: &fakeApprovalBusCloser{order: &order}, client: &fakeRedisClientCloser{order: &order}}, shutdownTimeout: time.Second}, signals)
			if !errors.Is(err, assessmentErr) {
				t.Fatalf("assessment failure not reported: %v", err)
			}
			if listenerFails && !errors.Is(err, listenerErr) {
				t.Fatalf("listener failure lost: %v", err)
			}
			want := "assessment-stop,http,tasks,assessment-wait,bus,redis"
			if got := strings.Join(order, ","); got != want {
				t.Fatalf("shutdown order=%s want=%s", got, want)
			}
		})
	}
}
