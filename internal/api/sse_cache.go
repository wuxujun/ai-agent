package api

import (
	"container/list"
	"time"
)

const maxStickyEvents = 1024
const maxStickyEventBytes = 64 << 10

// All entries share one TTL, so insertion order is expiry order. One timer and
// one queue node per cached task bound both background work and retained state.
func (b *EventBus) rememberStickyLocked(id string, event StepEvent) {
	b.removeStickyLocked(id)
	if event.Approval != nil || len(id)+len(event.TaskID)+len(event.Final)+len(event.Token)+len(event.ErrorMessage)+len(event.ErrorCode) > maxStickyEventBytes {
		b.scheduleStickyExpiryLocked()
		return // Live delivery and the Store backstop still carry the complete event.
	}
	if b.sticky == nil {
		b.sticky = make(map[string]stickyEvent)
	}
	if b.stickyOrder == nil {
		b.stickyOrder = list.New()
		b.stickyNodes = make(map[string]*list.Element)
	}
	b.sticky[id] = stickyEvent{event: event, expiresAt: b.nowFunc().Add(stickyTerminalTTL)}
	b.stickyNodes[id] = b.stickyOrder.PushBack(id)
	for len(b.sticky) > maxStickyEvents {
		b.removeStickyLocked(b.stickyOrder.Front().Value.(string))
	}
	b.scheduleStickyExpiryLocked()
}
func (b *EventBus) removeStickyLocked(id string) {
	delete(b.sticky, id)
	if node := b.stickyNodes[id]; node != nil {
		b.stickyOrder.Remove(node)
		delete(b.stickyNodes, id)
	}
}
func (b *EventBus) pruneStickyLocked() {
	if b.stickyOrder == nil {
		return
	}
	now := b.nowFunc()
	for front := b.stickyOrder.Front(); front != nil; front = b.stickyOrder.Front() {
		id := front.Value.(string)
		if now.Before(b.sticky[id].expiresAt) {
			break
		}
		b.removeStickyLocked(id)
	}
	b.scheduleStickyExpiryLocked()
}
func (b *EventBus) scheduleStickyExpiryLocked() {
	if b.stickyTimer != nil {
		b.stickyTimer.Stop()
		b.stickyTimer = nil
	}
	if b.stickyOrder == nil || b.stickyOrder.Len() == 0 {
		return
	}
	id := b.stickyOrder.Front().Value.(string)
	delay := b.sticky[id].expiresAt.Sub(b.nowFunc())
	b.stickyTimer = time.AfterFunc(max(0, delay), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.pruneStickyLocked()
	})
}
