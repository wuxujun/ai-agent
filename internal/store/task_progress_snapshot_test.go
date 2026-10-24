package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func progressTraceFixture() types.StepTrace {
	at := time.Now()
	duration := int64(12)
	return types.StepTrace{Step: 3, Goal: "goal", Action: "read_file", Query: "query", Observation: "observation", Evidence: []types.Evidence{{Path: "file", Lines: []string{"line"}, Query: "evidence query"}}, Error: "error", AgentRole: "planner", TokenUsage: types.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, OccurredAt: &at, DurationMS: &duration}
}

func TestProgressTraceEqualityDetectsEveryField(t *testing.T) {
	mutations := map[string]func(*types.StepTrace){
		"Step": func(v *types.StepTrace) { v.Step++ }, "Goal": func(v *types.StepTrace) { v.Goal += "x" }, "Action": func(v *types.StepTrace) { v.Action += "x" }, "Query": func(v *types.StepTrace) { v.Query += "x" }, "Observation": func(v *types.StepTrace) { v.Observation += "x" }, "Evidence": func(v *types.StepTrace) { v.Evidence[0].Lines[0] += "x" }, "Error": func(v *types.StepTrace) { v.Error += "x" }, "AgentRole": func(v *types.StepTrace) { v.AgentRole = "writer" }, "TokenUsage": func(v *types.StepTrace) { v.TokenUsage.TotalTokens++ }, "OccurredAt": func(v *types.StepTrace) { *v.OccurredAt = v.OccurredAt.Add(time.Second) }, "DurationMS": func(v *types.StepTrace) { *v.DurationMS++ },
	}
	// Adding fields requires explicitly reviewing comparison and detachment;
	// silently ignoring a future field would incorrectly skip durable updates.
	typ := reflect.TypeFor[types.StepTrace]()
	if typ.NumField() != len(mutations) {
		t.Fatal("StepTrace schema changed; review checkpoint comparison and clone")
	}
	for i := 0; i < typ.NumField(); i++ {
		if mutations[typ.Field(i).Name] == nil {
			t.Fatalf("uncovered field %s", typ.Field(i).Name)
		}
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			original := progressTraceFixture()
			detached := cloneProgressTrace(original)
			if !equalProgressTrace(original, detached) {
				t.Fatal("clone changed equality")
			}
			mutate(&original)
			if reflect.DeepEqual(original, detached) || equalProgressTrace(original, detached) {
				t.Fatal("mutation was not detached/detected")
			}
		})
	}
	evidenceType := reflect.TypeFor[types.Evidence]()
	if evidenceType.NumField() != 3 || evidenceType.Field(0).Name != "Path" || evidenceType.Field(1).Name != "Lines" || evidenceType.Field(2).Name != "Query" {
		t.Fatal("Evidence schema changed; review checkpoint clone/comparison")
	}
}

func TestProgressTraceEqualityPreservesRepresentations(t *testing.T) {
	base := progressTraceFixture()
	variants := []types.StepTrace{base, {}, {Evidence: []types.Evidence{}}, {Evidence: []types.Evidence{{}}}, {Evidence: []types.Evidence{{Lines: []string{}}}}}
	for _, path := range []string{"different path", ""} {
		v := cloneProgressTrace(base)
		v.Evidence[0].Path = path
		variants = append(variants, v)
	}
	v := cloneProgressTrace(base)
	v.Evidence[0].Query = "different"
	variants = append(variants, v)
	stripped := base.OccurredAt.Round(0)
	v = cloneProgressTrace(base)
	v.OccurredAt = &stripped
	variants = append(variants, v)
	for range 2 {
		at := time.Date(2026, 10, 24, 1, 2, 3, 0, time.FixedZone("independent", 3780))
		v = cloneProgressTrace(base)
		v.OccurredAt = &at
		variants = append(variants, v)
	}
	for i, a := range variants {
		for j, b := range variants {
			if got, want := equalProgressTrace(a, b), reflect.DeepEqual(a, b); got != want {
				t.Fatalf("pair %d/%d got=%v want=%v", i, j, got, want)
			}
		}
	}
	for _, v := range variants {
		if !reflect.DeepEqual(v, cloneProgressTrace(v)) {
			t.Fatal("clone changed nil/empty or timing representation")
		}
	}
}

func FuzzProgressTraceEquality(f *testing.F) {
	for _, pair := range [][2]types.StepTrace{{progressTraceFixture(), progressTraceFixture()}, {}, {progressTraceFixture(), {}}, {{Evidence: []types.Evidence{}}, {}}} {
		a, _ := json.Marshal(pair[0])
		b, _ := json.Marshal(pair[1])
		f.Add(a, b)
	}
	f.Fuzz(func(t *testing.T, aData, bData []byte) {
		var a, b types.StepTrace
		if json.Unmarshal(aData, &a) != nil || json.Unmarshal(bData, &b) != nil {
			return
		}
		if equalProgressTrace(a, b) != reflect.DeepEqual(a, b) {
			t.Fatal("comparison differs from DeepEqual")
		}
		if !reflect.DeepEqual(a, cloneProgressTrace(a)) {
			t.Fatal("clone differs")
		}
	})
}

func TestTaskProgressAdvanceSharesOnlyImmutableHistory(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := capacityTask(progressTraceBlockSize + 1)
	if err := st.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	before := CaptureTaskProgress(st, task)
	oldCount := before.trace.count
	row := progressTraceFixture()
	at := row.OccurredAt.UTC()
	row.OccurredAt = &at // SQL persists UTC wall time, without the monotonic clock.
	task.Trace = append(task.Trace, row)
	if err := SaveTaskProgress(t.Context(), st, task, before); err != nil {
		t.Fatal(err)
	}
	next := AdvanceTaskProgress(st, task, before)
	if next == nil || next.version != task.MaintenanceVersion || !next.trace.matches(task.Trace) {
		t.Fatal("invalid advanced checkpoint")
	}
	if &before.trace.blocks[0][0] != &next.trace.blocks[0][0] {
		t.Fatal("complete immutable block was copied")
	}
	if &before.trace.blocks[1][0] == &next.trace.blocks[1][0] || before.trace.count != oldCount || len(before.trace.blocks[1]) != 1 {
		t.Fatal("partial tail mutated in place")
	}
	task.Trace[0].Evidence[0].Lines[0] = "rewritten"
	if next.trace.matches(task.Trace) {
		t.Fatal("shared history is aliased with mutable task")
	}
	if err := SaveTaskProgress(t.Context(), st, task, next); err != nil {
		t.Fatal(err)
	}
	rewritten := AdvanceTaskProgress(st, task, next)
	if rewritten == nil || !rewritten.trace.matches(task.Trace) || &rewritten.trace.blocks[0][0] == &next.trace.blocks[0][0] {
		t.Fatal("rewrite did not reset detached checkpoint")
	}
	loaded, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !reflect.DeepEqual(loaded.Trace, task.Trace) {
		t.Fatalf("rewritten history not saved: %v", err)
	}
}

func TestTaskProgressAdvanceConcurrentBranches(t *testing.T) {
	// Pure acknowledged-snapshot fixtures; SQL version/rollback behavior is tested
	// separately. Sharing a parent must not expose writable tail storage.
	st := &SQLiteStore{}
	task := capacityTask(progressTraceBlockSize + 1)
	task.MaintenanceVersion = 1
	parent := CaptureTaskProgress(st, task)
	const branches = 16
	children := make([]*TaskProgressCheckpoint, branches)
	var wg sync.WaitGroup
	for i := range branches {
		wg.Go(func() {
			fork := types.CloneTask(task)
			fork.MaintenanceVersion = 2
			fork.Trace = append(fork.Trace, types.StepTrace{Observation: fmt.Sprint(i)})
			children[i] = AdvanceTaskProgress(st, fork, parent)
		})
	}
	wg.Wait()
	for i, child := range children {
		if child.trace.blocks[1][1].Observation != fmt.Sprint(i) {
			t.Fatal("concurrent branch overwrote another checkpoint")
		}
	}
	if parent.trace.count != len(task.Trace) || !parent.trace.matches(task.Trace) {
		t.Fatal("parent checkpoint mutated")
	}
}

func TestTaskProgressAdvanceFallsBackAcrossBindings(t *testing.T) {
	first, second := &SQLiteStore{}, &SQLiteStore{}
	original := capacityTask(progressTraceBlockSize + 1)
	original.MaintenanceVersion = 1
	parent := CaptureTaskProgress(first, original)
	for _, tc := range []struct {
		name    string
		owner   Store
		version int64
		id      string
	}{
		{"other store", second, 2, original.ID}, {"other task", first, 2, "other"}, {"same version", first, 1, original.ID}, {"skipped version", first, 3, original.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := types.CloneTask(original)
			task.ID = tc.id
			task.MaintenanceVersion = tc.version
			got := AdvanceTaskProgress(tc.owner, task, parent)
			if got == nil || got.owner != tc.owner || got.taskID != tc.id || got.version != tc.version || !got.trace.matches(task.Trace) {
				t.Fatal("fallback did not capture current acknowledged snapshot")
			}
			if &got.trace.blocks[0][0] == &parent.trace.blocks[0][0] {
				t.Fatal("reused history across binding boundary")
			}
		})
	}
	if got := AdvanceTaskProgress(NewMemoryStore(), original, parent); got != nil {
		t.Fatal("memory store unexpectedly gained progress capability")
	}
	if got := AdvanceTaskProgress(first, nil, parent); got != nil {
		t.Fatal("nil task captured")
	}
	ownedDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ownedDir, 0700); err != nil {
		t.Fatal(err)
	}
	owned, err := NewSQLiteStoreWithExecutionGate(filepath.Join(ownedDir, "owned.db"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if got := AdvanceTaskProgress(owned, original, parent); got != nil {
		t.Fatal("owned recovery runtime gained default progress path")
	}
}
