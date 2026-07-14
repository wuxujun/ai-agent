package memory_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestLocalEmbeddingAndSimilarity(t *testing.T) {
	ctx := context.Background()

	// Compute embeddings
	emb1, err := memory.GetEmbedding(ctx, "find the postgres database configuration file")
	if err != nil {
		t.Fatalf("failed to get embedding 1: %v", err)
	}

	emb2, err := memory.GetEmbedding(ctx, "lookup pg db configuration parameters")
	if err != nil {
		t.Fatalf("failed to get embedding 2: %v", err)
	}

	emb3, err := memory.GetEmbedding(ctx, "run tests on the project repository")
	if err != nil {
		t.Fatalf("failed to get embedding 3: %v", err)
	}

	// Verify lengths (should be non-empty and of identical size)
	if len(emb1) == 0 || len(emb2) != len(emb1) || len(emb3) != len(emb1) {
		t.Errorf("expected non-empty embeddings of matching sizes, got len1=%d, len2=%d, len3=%d", len(emb1), len(emb2), len(emb3))
	}

	// Calculate similarities
	sim12 := memory.CosineSimilarity(emb1, emb2)
	sim13 := memory.CosineSimilarity(emb1, emb3)

	// Since local embedding is based on word-overlap, "postgres database configuration file"
	// and "pg db configuration parameters" share "configuration" (and potentially hashes collision, but usually config is the main overlap).
	// "run tests on the project repository" shares nothing.
	// Therefore, sim12 should be greater than sim13.
	t.Logf("Similarity 1-2 (related): %f", sim12)
	t.Logf("Similarity 1-3 (unrelated): %f", sim13)

	if sim12 < sim13 {
		t.Errorf("expected related queries to have higher similarity: sim12=%f, sim13=%f", sim12, sim13)
	}
}

func TestSummarizeTask(t *testing.T) {
	task := &types.Task{
		ID:   "task-xyz",
		Goal: "find config file and check api key",
		Trace: []types.StepTrace{
			{
				Step:        1,
				AgentRole:   types.AgentRolePlanner,
				Action:      "plan",
				Observation: "planned 3 steps",
			},
			{
				Step:        2,
				AgentRole:   types.AgentRoleResearcher,
				Action:      "find_files",
				Observation: "found config.yaml and credentials.json",
			},
			{
				Step:        3,
				AgentRole:   types.AgentRoleResearcher,
				Action:      "read_file",
				Observation: "read config.yaml: found API_KEY=secret-xyz",
			},
		},
		FinalAnswer: "The API key is secret-xyz in config.yaml",
	}

	summary := memory.SummarizeTask(task)
	t.Logf("Task Summary:\n%s", summary)

	// Verify it contains researcher actions/observations but NOT planner step
	if !strings.Contains(summary, "found config.yaml") {
		t.Errorf("expected summary to contain researcher findings, got: %s", summary)
	}
	if strings.Contains(summary, "planned 3 steps") {
		t.Errorf("expected summary to filter out planner trace entries, got: %s", summary)
	}
}

func TestCreateMemoryFromTask(t *testing.T) {
	ctx := context.Background()
	task := &types.Task{
		ID:   "task-xyz",
		Goal: "find config file and check api key",
		Trace: []types.StepTrace{
			{
				Step:        1,
				AgentRole:   types.AgentRoleResearcher,
				Action:      "find_files",
				Observation: "found config.yaml",
			},
		},
		FinalAnswer: "The API key is secret-xyz",
	}

	mem, err := memory.CreateMemoryFromTask(ctx, task)
	if err != nil {
		t.Fatalf("failed to create memory from task: %v", err)
	}

	if mem.ID != "mem-task-xyz" {
		t.Errorf("expected memory ID 'mem-task-xyz', got %q", mem.ID)
	}
	if mem.Goal != task.Goal {
		t.Errorf("expected goal %q, got %q", task.Goal, mem.Goal)
	}
	if mem.FinalAnswer != task.FinalAnswer {
		t.Errorf("expected final answer %q, got %q", task.FinalAnswer, mem.FinalAnswer)
	}
	if !strings.Contains(mem.KeyFindings, "found config.yaml") {
		t.Errorf("expected key findings to include findings summary, got %q", mem.KeyFindings)
	}
	if len(mem.Embedding) == 0 {
		t.Errorf("expected embedding to be computed (non-empty), got length %d", len(mem.Embedding))
	}
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	// Zero vectors or empty slices
	if sim := memory.CosineSimilarity(nil, nil); sim != 0 {
		t.Errorf("expected similarity of nil vectors to be 0, got %f", sim)
	}

	// Mismatched lengths
	if sim := memory.CosineSimilarity([]float32{1.0}, []float32{1.0, 2.0}); sim != 0 {
		t.Errorf("expected similarity of mismatched lengths to be 0, got %f", sim)
	}

	// All-zero vectors
	if sim := memory.CosineSimilarity([]float32{0.0, 0.0}, []float32{0.0, 0.0}); sim != 0 {
		t.Errorf("expected similarity of zero vectors to be 0, got %f", sim)
	}

	// Exact same vectors
	v := []float32{3.0, 4.0} // Magnitude 5
	sim := memory.CosineSimilarity(v, v)
	if math.Abs(float64(sim)-1.0) > 1e-6 {
		t.Errorf("expected similarity of identical vectors to be 1.0, got %f", sim)
	}
}

func TestDeduplicateMemories(t *testing.T) {
	mems := []types.Memory{
		{ID: "mem-1", TaskID: "task-1", Goal: "Find files", FinalAnswer: "Found A"},
		{ID: "mem-1", TaskID: "task-1", Goal: "Find files", FinalAnswer: "Found A"},        // Duplicate ID
		{ID: "mem-2", TaskID: "task-1", Goal: "Find files", FinalAnswer: "Found A"},        // Duplicate TaskID
		{ID: "mem-3", TaskID: "task-2", Goal: "find files", FinalAnswer: "Found B"},        // Duplicate Goal (case-insensitive)
		{ID: "mem-4", TaskID: "task-3", Goal: "Lookup users", FinalAnswer: "found a"},      // Duplicate FinalAnswer (case-insensitive)
		{ID: "mem-5", TaskID: "task-4", Goal: "Unique Goal", FinalAnswer: "Unique Answer"}, // Unique
	}

	deduped := memory.DeduplicateMemories(mems)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 unique memories, got %d: %+v", len(deduped), deduped)
	}

	if deduped[0].ID != "mem-1" || deduped[1].ID != "mem-5" {
		t.Errorf("unexpected deduped result order or keys: %+v", deduped)
	}
}

func TestSemanticDeduplicateMemories(t *testing.T) {
	emb1 := []float32{1.0, 0.0, 0.0}
	emb2 := []float32{0.99, 0.1, 0.0} // High similarity (approx 0.995)
	emb3 := []float32{0.0, 1.0, 0.0}  // Low similarity (0.0)

	mems := []types.Memory{
		{ID: "mem-1", TaskID: "task-1", Goal: "Goal 1", FinalAnswer: "Answer 1", Embedding: emb1},
		{ID: "mem-2", TaskID: "task-2", Goal: "Goal 2", FinalAnswer: "Answer 2", Embedding: emb2}, // Semantically redundant
		{ID: "mem-3", TaskID: "task-3", Goal: "Goal 3", FinalAnswer: "Answer 3", Embedding: emb3}, // Semantically distinct
	}

	deduped := memory.DeduplicateMemories(mems)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 memories after semantic deduplication, got %d", len(deduped))
	}

	if deduped[0].ID != "mem-1" || deduped[1].ID != "mem-3" {
		t.Errorf("unexpected deduped result: %+v", deduped)
	}
}

func TestApplyTimeDecay(t *testing.T) {
	now := time.Now()
	timestamp1 := now.Add(-10 * time.Hour) // 10 hours ago
	timestamp2 := now.Add(-50 * time.Hour) // 50 hours ago

	// 1. Decay rate is 0.0 -> no decay should occur
	score0 := memory.ApplyTimeDecay(1.0, timestamp1, now, 0.0)
	if score0 != 1.0 {
		t.Errorf("expected score 1.0, got %f with decay rate 0.0", score0)
	}

	// 2. Decay rate is positive (e.g. 0.01 per hour)
	score1 := memory.ApplyTimeDecay(1.0, timestamp1, now, 0.01)
	score2 := memory.ApplyTimeDecay(1.0, timestamp2, now, 0.01)

	// Since timestamp2 is older, its score should be decayed more.
	if score2 >= score1 {
		t.Errorf("expected older memory to have lower decayed score: score2=%f, score1=%f", score2, score1)
	}

	// Verify exact values (exp(-0.01 * 10) = ~0.9048)
	expected1 := float32(math.Exp(-0.01 * 10))
	if math.Abs(float64(score1-expected1)) > 1e-5 {
		t.Errorf("expected score1 around %f, got %f", expected1, score1)
	}

	// 3. Score is decayed to zero if time is extremely large
	timestamp3 := now.Add(-100000 * time.Hour)
	score3 := memory.ApplyTimeDecay(1.0, timestamp3, now, 0.1)
	if score3 > 1e-5 {
		t.Errorf("expected score3 to be close to 0, got %f", score3)
	}

	// 4. Future timestamp (negative hours elapsed) should not decay (treated as 0 elapsed)
	futureTimestamp := now.Add(2 * time.Hour)
	scoreFuture := memory.ApplyTimeDecay(1.0, futureTimestamp, now, 0.01)
	if scoreFuture != 1.0 {
		t.Errorf("expected future memory to have score 1.0 (no decay), got %f", scoreFuture)
	}
}
