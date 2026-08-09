package orchestrator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestApprovalBusAcrossRedisClients(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_EXTERNAL_INTEGRATION") != "true" {
		t.Skip("set AI_AGENT_RUN_EXTERNAL_INTEGRATION=true to run tests against dedicated external services")
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	receiverClient := redis.NewClient(opts)
	senderClient := redis.NewClient(opts)
	defer receiverClient.Close()
	defer senderClient.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	receiver := NewApprovalBus(receiverClient)
	receiver.Start(ctx)
	defer receiver.Close()
	sender := NewApprovalBus(senderClient)

	assertApprovalBusDelivery(t, ctx, sender, true)
	assertApprovalBusDelivery(t, ctx, sender, false)
	assertCancelBusDelivery(t, ctx, receiver, sender)
}

func assertApprovalBusDelivery(t *testing.T, ctx context.Context, sender *ApprovalBus, approved bool) {
	t.Helper()
	taskID := "integration-approval-" + uuid.NewString()
	request := &types.ApprovalRequest{TaskID: taskID, Action: "write_file"}
	approvalID, resultCh := RegisterApproval(taskID, request)
	defer RemoveApproval(approvalID)
	wantMessage := "cross-client-approved"
	if !approved {
		wantMessage = "cross-client-rejected"
	}

	publishUntilDone := time.NewTicker(25 * time.Millisecond)
	defer publishUntilDone.Stop()
	for {
		select {
		case result := <-resultCh:
			if result.Approved != approved || result.Message != wantMessage {
				t.Fatalf("approval result = %+v", result)
			}
			return
		case <-publishUntilDone.C:
			if err := sender.PublishApproval(ctx, approvalID, taskID, types.ApprovalResult{Approved: approved, Message: wantMessage}); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("cross-client approval was not delivered: %v", ctx.Err())
		}
	}
}

func assertCancelBusDelivery(t *testing.T, ctx context.Context, receiver, sender *ApprovalBus) {
	t.Helper()
	taskID := "integration-cancel-" + uuid.NewString()
	cancelSignals := receiver.SubscribeCancelSignals(ctx)
	publishUntilDone := time.NewTicker(25 * time.Millisecond)
	defer publishUntilDone.Stop()
	for {
		select {
		case gotTaskID, ok := <-cancelSignals:
			if !ok {
				t.Fatalf("cancel subscription closed before delivery: %v", ctx.Err())
			}
			if gotTaskID != taskID {
				t.Fatalf("cancel task ID = %q, want %q", gotTaskID, taskID)
			}
			return
		case <-publishUntilDone.C:
			if err := sender.PublishCancel(ctx, taskID); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("cross-client cancel was not delivered: %v", ctx.Err())
		}
	}
}
