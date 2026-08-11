package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestExpirePendingApprovalsTransitionsStaleRecord(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) { cfg.Approval.TTLSeconds = 1 })
	t.Cleanup(restore)
	st := store.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	collector := metrics.NewCollector()
	eng := &orchestrator.Engine{Store: st, ApprovalCodec: codec, Metrics: collector}
	task := &types.Task{ID: "expiry-scan-task", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "expiry-scan-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:       types.ApprovalRequest{ID: "expiry-scan-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalPending, CreatedAt: time.Now().Add(-2 * time.Second),
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	expirePendingApprovals(context.Background(), st, st, eng)
	stored, err := st.GetApproval(context.Background(), approval.ID, approval.TenantID)
	if err != nil || stored.Status != types.ApprovalExpired {
		t.Fatalf("approval = %#v, %v", stored, err)
	}
	if collector.Snapshot().DurableApprovalsExpired != 1 {
		t.Fatalf("metrics = %+v", collector.Snapshot())
	}
}

func TestBuildStoreSelectsMemory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Store.Type = "memory"
	st, err := buildStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*store.MemoryStore); !ok {
		t.Fatalf("buildStore() type = %T, want *store.MemoryStore", st)
	}
}

func TestBuildStoreRequiresExternalDSN(t *testing.T) {
	for _, storeType := range []string{"postgres", "redis"} {
		t.Run(storeType, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Store.Type = storeType
			if st, err := buildStore(cfg); err == nil || st != nil {
				t.Fatalf("buildStore() = %T, %v; want nil store and DSN error", st, err)
			}
		})
	}
}

func TestBuildStoreCreatesSQLiteAtConfiguredPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Store.Type = "sqlite"
	cfg.Store.DSN = filepath.Join(t.TempDir(), "agent.db")
	st, err := buildStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*store.SQLiteStore); !ok {
		t.Fatalf("buildStore() type = %T, want *store.SQLiteStore", st)
	}
}

func TestResolveApprovalBusDSN(t *testing.T) {
	tests := []struct {
		name      string
		storeType string
		storeDSN  string
		envDSN    string
		want      string
	}{
		{name: "Redis store uses its DSN", storeType: "redis", storeDSN: "redis://store", envDSN: "redis://dedicated", want: "redis://store"},
		{name: "Redis store does not fall back", storeType: "redis", envDSN: "redis://dedicated", want: ""},
		{name: "other store uses dedicated bus", storeType: "sqlite", storeDSN: "agent.db", envDSN: "redis://dedicated", want: "redis://dedicated"},
		{name: "bus is optional", storeType: "memory", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Store.Type = tt.storeType
			cfg.Store.DSN = tt.storeDSN
			got := resolveApprovalBusDSN(cfg, func(string) string { return tt.envDSN })
			if got != tt.want {
				t.Fatalf("resolveApprovalBusDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildAppCreatesDefaultHTTPTransport(t *testing.T) {
	t.Setenv("AI_AGENT_REDIS_BUS_URL", "")
	cfg := &config.Config{}
	cfg.Store.Type = "memory"
	st := store.NewMemoryStore()
	defer st.Close()
	mc := metrics.NewCollector()
	eng := &orchestrator.Engine{Store: st, Metrics: mc}
	app := buildApp(cfg, st, eng, mc, llmcore.NewDefaultRuntime(mc))
	defer app.bus.Close()

	if app.server.Addr != "127.0.0.1:8080" {
		t.Fatalf("server address = %q", app.server.Addr)
	}
	if app.tasks == nil || app.server.Handler == nil {
		t.Fatal("buildApp did not construct HTTP handler dependencies")
	}
	if eng.EventCallback == nil || eng.ApprovalCallback == nil || eng.StepCallback == nil || eng.TokenCallback == nil {
		t.Fatal("buildApp did not wire engine event callbacks")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ping", nil)
	app.server.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"message":"pong"}` {
		t.Fatalf("GET /ping = %d %s", recorder.Code, recorder.Body.String())
	}
}
