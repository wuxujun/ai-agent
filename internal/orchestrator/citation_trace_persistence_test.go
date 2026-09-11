package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestCitationAuditSurvivesSQLRoundTripWithoutSpendingSteps(t *testing.T) {
	st, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, CitationVerifier: &stubCitationVerifier{result: &planner.CitationVerification{Supported: true, VerifiedAnswer: "verified [E1]"}, usage: types.TokenUsage{TotalTokens: 6}}, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "citation-roundtrip", Goal: "fixture", FinalAnswer: "fixture", Status: types.StatusRunning, StepCount: 1, MaxSteps: 1, Trace: []types.StepTrace{{Step: 1, Action: "read_file", Evidence: []types.Evidence{{Path: "source", Lines: []string{"fact"}}}}}}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	e.verifyCitations(context.Background(), task)
	if len(task.Trace) != 2 {
		t.Fatal("fixture did not append citation audit")
	}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Trace) != 2 || got.Trace[1].Action != "citation_verify" || got.Trace[1].TokenUsage.TotalTokens != 6 || got.StepCount != 1 {
		t.Fatalf("citation audit lost or consumed execution budget: %+v", got)
	}
}
