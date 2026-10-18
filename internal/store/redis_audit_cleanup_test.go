package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExternalRedisTaskDeletionCleansAllAssociatedIndexes(t *testing.T) {
	requireExternalIntegration(t)
	rawURL := os.Getenv("TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	st, err := NewRedisStoreFromURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	id := "audit-delete-" + uuid.NewString()
	tenant := id + "-tenant"
	session := id + "-session"
	task := &types.Task{ID: id, TenantID: tenant, SessionID: session, Status: types.StatusCreated}
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	record := &types.DurableApproval{ID: id, TaskID: id, TenantID: tenant, Status: types.ApprovalPending, ActionPayload: []byte("fixture"), Request: types.ApprovalRequest{ID: id, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}}
	if err := st.CreateApproval(ctx, record); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"new", "legacy"} {
		mem := &types.Memory{ID: id + "-" + suffix, TaskID: id, TenantID: tenant, SessionID: session, Timestamp: time.Now()}
		if suffix == "new" {
			if err := st.SaveMemory(ctx, mem); err != nil {
				t.Fatal(err)
			}
		} else {
			data, _ := json.Marshal(mem)
			// Model a pre-upgrade value with no per-task memory index.
			if err := st.client.Set(ctx, st.memoryKey(mem.ID), data, 0).Err(); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"memories:index", "memories:tenant:" + tenant, "memories:session:" + tenant + ":" + session} {
				if err := st.client.ZAdd(ctx, key, redis.Z{Score: 1, Member: mem.ID}).Err(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// A rejected cross-tenant deletion must not mutate the task memory index.
	if removed, err := st.DeleteMemory(ctx, id+"-new", "other-tenant"); err != nil || removed {
		t.Fatal("cross-tenant delete", removed, err)
	}
	if n, err := st.client.ZCard(ctx, "memories:task:"+id).Result(); err != nil || n != 1 {
		t.Fatal("cross-tenant request damaged index", n, err)
	}
	// Force the one-time old-data migration on this dedicated integration service.
	if err := st.client.Del(ctx, "memories:task-indexed:v1").Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.DeleteTask(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := st.GetApproval(ctx, id, tenant); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("orphan approval", err)
	}
	for _, key := range []string{approvalTaskIndex(tenant, id), approvalTenantIndex(tenant), approvalTenantStatusIndex(tenant, types.ApprovalPending), "memories:task:" + id, "memories:tenant:" + tenant, "memories:session:" + tenant + ":" + session} {
		if n, err := st.client.ZCard(ctx, key).Result(); err != nil || n != 0 {
			t.Fatal("orphan index", key, n, err)
		}
	}
	for _, suffix := range []string{"new", "legacy"} {
		if n, err := st.client.Exists(ctx, st.memoryKey(id+"-"+suffix)).Result(); err != nil || n != 0 {
			t.Fatal("orphan memory", n, err)
		}
	}
	lateCtx := context.WithValue(ctx, indexedMemoryKey{}, true)
	if err := st.SaveMemory(lateCtx, &types.Memory{ID: id + "-late", TaskID: id, TenantID: tenant, SessionID: session, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.client.Exists(ctx, st.memoryKey(id+"-late")).Result(); err != nil || n != 0 {
		t.Fatal("deleted memory resurrected", n, err)
	}
}
