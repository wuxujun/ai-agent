package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/types"
)

var busLog = logger.Component("approval-bus")

// ApprovalBus provides cross-instance signalling for approval and cancellation
// events via Redis Pub/Sub. It is an optional component: when nil (no Redis
// configured), the orchestrator falls back to in-process channels only.
//
// Lifecycle:
//
//	bus := NewApprovalBus(redisClient)
//	bus.Start(ctx)          // subscribe loop in background goroutine
//	defer bus.Close()
type ApprovalBus struct {
	client *redis.Client
	sub    *redis.PubSub
	done   chan struct{}
}

// busMessage is the wire format published on the Redis channel.
type busMessage struct {
	// Type is "approve" or "cancel".
	Type string `json:"type"`

	// ApprovalID identifies the specific pending approval (may be empty for
	// cancel-by-task signals).
	ApprovalID string `json:"approval_id,omitempty"`

	// TaskID is always set; used for cancel signals and fallback resolution.
	TaskID string `json:"task_id"`

	// Result carries approve/reject payload. Only set when Type == "approve".
	Result *types.ApprovalResult `json:"result,omitempty"`
}

const approvalChannel = "ai-agent:approvals"
const cancelChannel = "ai-agent:cancels"

// NewApprovalBus creates an ApprovalBus backed by the given Redis client.
// Call Start to begin processing incoming messages.
func NewApprovalBus(client *redis.Client) *ApprovalBus {
	return &ApprovalBus{
		client: client,
		done:   make(chan struct{}),
	}
}

// Start subscribes to the Redis channels and dispatches incoming messages to
// local in-process approval channels. It runs in the background until ctx is
// cancelled or Close is called.
func (b *ApprovalBus) Start(ctx context.Context) {
	b.sub = b.client.Subscribe(ctx, approvalChannel, cancelChannel)
	go b.loop(ctx)
}

// Close unsubscribes and waits for the loop to exit.
func (b *ApprovalBus) Close() {
	if b.sub != nil {
		_ = b.sub.Close()
	}
	<-b.done
}

// PublishApproval broadcasts an approval result to all instances in the
// cluster. Other instances will forward the message into their local channel
// if they hold the matching approvalID.
func (b *ApprovalBus) PublishApproval(ctx context.Context, approvalID, taskID string, result types.ApprovalResult) error {
	msg := busMessage{
		Type:       "approve",
		ApprovalID: approvalID,
		TaskID:     taskID,
		Result:     &result,
	}
	return b.publish(ctx, approvalChannel, msg)
}

// PublishCancel broadcasts a cancellation signal for taskID to all instances.
func (b *ApprovalBus) PublishCancel(ctx context.Context, taskID string) error {
	msg := busMessage{
		Type:   "cancel",
		TaskID: taskID,
	}
	return b.publish(ctx, cancelChannel, msg)
}

func (b *ApprovalBus) publish(ctx context.Context, channel string, msg busMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("approval bus marshal: %w", err)
	}
	publishCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return b.client.Publish(publishCtx, channel, payload).Err()
}

// loop receives Redis messages and dispatches them into local approval or
// cancel channels. It exits when the subscription is closed.
func (b *ApprovalBus) loop(ctx context.Context) {
	defer close(b.done)
	ch := b.sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return // subscription closed
			}
			b.dispatchApproval(msg)
		}
	}
}

func (b *ApprovalBus) dispatchApproval(msg *redis.Message) {
	if msg.Channel != approvalChannel {
		return
	}
	var bm busMessage
	if err := json.Unmarshal([]byte(msg.Payload), &bm); err != nil {
		busLog.Warn("approval bus: failed to unmarshal message", "error", err)
		return
	}
	if bm.Type != "approve" || bm.Result == nil {
		return
	}
	// Try by specific approval ID first; fall back to task's unique pending if approval ID is empty.
	var resolved bool
	if bm.ApprovalID != "" {
		resolved = ResolveApprovalByID(bm.ApprovalID, *bm.Result)
		// If the specific ID didn't resolve, this is a stale/duplicate Pub/Sub
		// delivery (the approval was already handled by another instance, or the
		// message was re-delivered after a crash). We intentionally do NOT fall
		// through to ResolveApproval(taskID) here — that would risk auto-approving
		// a different, unrelated pending action on the same task (BUG_REPORT.md #3).
		if !resolved {
			busLog.Warn("approval bus: ignoring stale or duplicate message (approval already resolved or unknown)",
				"approval_id", bm.ApprovalID,
				"task_id", bm.TaskID,
			)
		}
	} else {
		resolved = ResolveApproval(bm.TaskID, *bm.Result)
	}
	if resolved {
		busLog.Info("approval bus: resolved approval from remote signal",
			"approval_id", bm.ApprovalID,
			"task_id", bm.TaskID,
			"approved", bm.Result.Approved,
		)
	}
}

// SubscribeCancelSignals returns a channel that emits task IDs whenever a
// remote cancel signal arrives from another instance. The caller is responsible
// for draining the channel and invoking the appropriate context cancellation.
// The returned channel is closed when ctx is done.
func (b *ApprovalBus) SubscribeCancelSignals(ctx context.Context) <-chan string {
	out := make(chan string, 32)
	go func() {
		defer close(out)
		sub := b.client.Subscribe(ctx, cancelChannel)
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var bm busMessage
				if err := json.Unmarshal([]byte(msg.Payload), &bm); err != nil {
					continue
				}
				if bm.Type == "cancel" {
					select {
					case out <- bm.TaskID:
					default:
						// drop if consumer is slow — cancel is best-effort
					}
				}
			}
		}
	}()
	return out
}
