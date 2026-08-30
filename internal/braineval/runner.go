package braineval

import (
	"context"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type Variant string

const (
	VariantBaseline Variant = "baseline"
	VariantBrain    Variant = "brain"
)

type Limits struct {
	BranchCandidates int
	EvidenceItems    int
	EvidenceBytes    int
}

type VariantOutput struct {
	Variant    Variant
	Candidates []Candidate
	Evidence   []types.Evidence
	Latency    time.Duration
	Err        string
	Limits     Limits
}

type PairResult struct {
	Case       Case
	Baseline   VariantOutput
	Candidate  VariantOutput
	Comparable bool
}

type scopeRuntime struct {
	memory    Retriever
	brain     Retriever
	retracted map[string]struct{}
}

type OfflineRunner struct {
	dataset Dataset
	corpus  *Corpus
	scopes  map[string]*scopeRuntime
}

// NewOfflineRunner creates isolated Memory and Brain retrievers for each
// validated dataset scope.
func NewOfflineRunner(dataset Dataset, corpus *Corpus) (*OfflineRunner, error) {
	if err := dataset.Validate(); err != nil {
		return nil, fmt.Errorf("validate dataset: %w", err)
	}
	if err := corpus.Validate(); err != nil {
		return nil, fmt.Errorf("validate corpus: %w", err)
	}
	memory, err := NewMemoryRetriever(context.Background(), corpus)
	if err != nil {
		return nil, fmt.Errorf("create memory retriever: %w", err)
	}
	brain, err := NewWikiRetriever(context.Background(), corpus)
	if err != nil {
		return nil, fmt.Errorf("create brain retriever: %w", err)
	}
	runner := &OfflineRunner{
		dataset: dataset,
		corpus:  corpus,
		scopes:  make(map[string]*scopeRuntime, len(dataset.Projects)),
	}
	for _, fixture := range dataset.Projects {
		project := corpus.Projects[fixture.Scope.Key()]
		if project == nil {
			return nil, fmt.Errorf("corpus is missing scope %q", fixture.Scope.Key())
		}
		retracted := make(map[string]struct{}, len(project.Retractions))
		for _, retraction := range project.Retractions {
			retracted[canonicalURI(retraction.URI)] = struct{}{}
		}
		runner.scopes[fixture.Scope.Key()] = &scopeRuntime{
			memory: memory, brain: brain, retracted: retracted,
		}
	}
	return runner, nil
}

// RunPair evaluates one case with matched limits. The baseline can access only
// Memory, while the candidate additionally accesses Brain.
func (r *OfflineRunner) RunPair(ctx context.Context, caseDef Case) (PairResult, error) {
	pair := PairResult{
		Case:      caseDef,
		Baseline:  newVariantOutput(VariantBaseline),
		Candidate: newVariantOutput(VariantBrain),
	}
	if err := ctx.Err(); err != nil {
		return pair, err
	}
	if r == nil {
		return pair, fmt.Errorf("offline runner is nil")
	}
	runtime, ok := r.scopes[caseDef.Scope.Key()]
	if !ok || runtime == nil {
		return pair, fmt.Errorf("unknown retrieval scope %q", caseDef.Scope.Key())
	}

	pair.Baseline = r.planEvidence(ctx, caseDef, VariantBaseline, runtime)
	if err := ctx.Err(); err != nil {
		return pair, err
	}
	pair.Candidate = r.planEvidence(ctx, caseDef, VariantBrain, runtime)
	if err := ctx.Err(); err != nil {
		return pair, err
	}
	pair.Comparable = pair.Baseline.Err == "" && pair.Candidate.Err == ""
	return pair, nil
}

type retrievalBranch struct {
	name      string
	retriever Retriever
}

func (r *OfflineRunner) planEvidence(ctx context.Context, caseDef Case, variant Variant, runtime *scopeRuntime) VariantOutput {
	started := time.Now()
	output := newVariantOutput(variant)
	defer func() { output.Latency = time.Since(started) }()

	branches := []retrievalBranch{{name: "memory", retriever: runtime.memory}}
	if variant == VariantBrain {
		branches = append(branches, retrievalBranch{name: "brain", retriever: runtime.brain})
	}
	rankedBranches := make([][]Candidate, 0, len(branches))
	fetchers := make(map[string]Retriever, len(branches))
	for _, branch := range branches {
		if err := ctx.Err(); err != nil {
			output.Err = err.Error()
			return output
		}
		if branch.retriever == nil {
			output.Err = fmt.Sprintf("%s retriever is unavailable", branch.name)
			return output
		}
		candidates, err := branch.retriever.Search(ctx, caseDef.Scope, caseDef.Query, BranchCandidateLimit)
		if err != nil {
			output.Err = fmt.Sprintf("%s search: %v", branch.name, err)
			return output
		}
		filtered := make([]Candidate, 0, len(candidates))
		for _, candidate := range candidates {
			candidate.URI = canonicalURI(candidate.URI)
			if _, retracted := runtime.retracted[candidate.URI]; retracted {
				continue
			}
			candidate.Branch = branch.name
			filtered = append(filtered, candidate)
		}
		for i := range filtered {
			filtered[i].Rank = i + 1
		}
		rankedBranches = append(rankedBranches, filtered)
		fetchers[branch.name] = branch.retriever
	}

	output.Candidates = MergeRRF(rankedBranches, RRFK)
	evidence, err := SelectEvidence(ctx, branchEvidenceRetriever{fetchers: fetchers}, caseDef.Scope, output.Candidates, output.Limits.EvidenceItems, output.Limits.EvidenceBytes)
	if err != nil {
		output.Err = fmt.Sprintf("select evidence: %v", err)
		return output
	}
	output.Evidence = evidence
	return output
}

func newVariantOutput(variant Variant) VariantOutput {
	return VariantOutput{
		Variant: variant,
		Limits: Limits{
			BranchCandidates: BranchCandidateLimit,
			EvidenceItems:    FinalEvidenceLimit,
			EvidenceBytes:    FinalEvidenceBytes,
		},
	}
}

type branchEvidenceRetriever struct {
	fetchers map[string]Retriever
}

func (r branchEvidenceRetriever) Search(context.Context, Scope, string, int) ([]Candidate, error) {
	return nil, fmt.Errorf("search is not supported after ranking")
}

func (r branchEvidenceRetriever) Fetch(ctx context.Context, scope Scope, candidate Candidate) (types.Evidence, error) {
	retriever, ok := r.fetchers[candidate.Branch]
	if !ok || retriever == nil {
		return types.Evidence{}, fmt.Errorf("no retriever for branch %q", candidate.Branch)
	}
	return retriever.Fetch(ctx, scope, candidate)
}
