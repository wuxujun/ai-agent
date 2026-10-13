package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskRestoreCommandImportAndVerify(t *testing.T) {
	source := store.NewMemoryStore()
	defer source.Close()
	task := &types.Task{ID: "offline-task", TenantID: "a", Status: types.StatusFailed, Goal: "private-goal", Trace: []types.StepTrace{{Observation: "private-trace"}}}
	if err := source.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ReadJointTaskExport(t.Context(), task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.EncodeTaskExport(t.Context(), snapshot, "memory", task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	file := filepath.Join(parent, "export.json")
	if err := os.WriteFile(file, artifact, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"import", "--file", file, "--tenant", "a", "--parent", parent}, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	var result restoreOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Directory == "" || result.Verification == nil || !result.Verification.Reopened || !result.Verification.Verified || result.Verification.RestoreReady {
		t.Fatal("wrong import report", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(t.Context(), []string{"verify", "--directory", result.Directory, "--tenant", "a", "--task", task.ID}, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	if bytes.Contains(out.Bytes(), []byte("private-")) {
		t.Fatal("CLI leaked payload")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(after, artifact) {
		t.Fatal("source export changed", err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(t.Context(), []string{"verify", "--directory", result.Directory, "--tenant", "b", "--task", task.ID}, &out, &errOut); code == 0 || bytes.Contains(errOut.Bytes(), []byte("private-")) {
		t.Fatal("wrong tenant verified")
	}
}

func TestTaskRestoreCommandInvalidArgumentsAndQuota(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "large.json")
	if err := os.WriteFile(file, bytes.Repeat([]byte("x"), 4097), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"import"}, {"import", "--file", file, "--tenant", "a", "--parent", parent, "--byte-budget", "4096"}, {"verify", "--directory", parent, "--tenant", "a", "--task", "x"}, {"verify", "--directory", parent, "--tenant", "a", "--task", "x", "--file", file}} {
		var out, errOut bytes.Buffer
		if code := run(t.Context(), args, &out, &errOut); code == 0 {
			t.Fatal("invalid command accepted", args)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatal("invalid command created directories", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"verify", "--directory", parent, "--tenant", "a", "--task", "x"}, &out, &errOut); code == 0 {
		t.Fatal("cancellation ignored")
	}
}
