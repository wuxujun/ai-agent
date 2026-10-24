package store

import (
	"reflect"
	"slices"

	"github.com/wuxujun/ai-agent/internal/types"
)

const progressTraceBlockSize = 256

// Blocks, rows and their nested evidence/timing values are immutable after
// capture. An advance copies only the block index and the partial tail block;
// historical blocks can therefore be shared by concurrent checkpoint readers.
type progressTraceSnapshot struct {
	blocks [][]types.StepTrace
	count  int
}

func (s progressTraceSnapshot) extend(tail []types.StepTrace) progressTraceSnapshot {
	if len(tail) == 0 {
		return s
	}
	result := progressTraceSnapshot{blocks: slices.Clone(s.blocks), count: s.count + len(tail)}
	if last := len(result.blocks) - 1; last >= 0 && len(result.blocks[last]) < progressTraceBlockSize {
		old := result.blocks[last]
		take := min(len(tail), progressTraceBlockSize-len(old))
		block := make([]types.StepTrace, len(old)+take)
		copy(block, old)
		for i := range take {
			block[len(old)+i] = cloneProgressTrace(tail[i])
		}
		result.blocks[last] = block
		tail = tail[take:]
	}
	for len(tail) > 0 {
		take := min(len(tail), progressTraceBlockSize)
		block := make([]types.StepTrace, take)
		for i := range take {
			block[i] = cloneProgressTrace(tail[i])
		}
		result.blocks = append(result.blocks, block)
		tail = tail[take:]
	}
	return result
}

func (s progressTraceSnapshot) matches(trace []types.StepTrace) bool {
	if s.count > len(trace) {
		return false
	}
	offset := 0
	for _, block := range s.blocks {
		for i := range block {
			if !equalProgressTrace(block[i], trace[offset+i]) {
				return false
			}
		}
		offset += len(block)
	}
	return true
}

func cloneProgressTrace(trace types.StepTrace) types.StepTrace {
	result := trace
	if trace.OccurredAt != nil {
		at := *trace.OccurredAt
		result.OccurredAt = &at
	}
	if trace.DurationMS != nil {
		duration := *trace.DurationMS
		result.DurationMS = &duration
	}
	if trace.Evidence != nil {
		result.Evidence = make([]types.Evidence, len(trace.Evidence))
		for i, e := range trace.Evidence {
			result.Evidence[i] = e
			if e.Lines != nil {
				result.Evidence[i].Lines = make([]string, len(e.Lines))
				copy(result.Evidence[i].Lines, e.Lines)
			}
		}
	}
	return result
}

// Keep the exact old DeepEqual semantics, including nil vs empty evidence and
// structurally equal time locations. Reflection is needed only on the uncommon
// path where time.Time's comparable representation differs.
func equalProgressTrace(a, b types.StepTrace) bool {
	if a.Step != b.Step || a.Goal != b.Goal || a.Action != b.Action || a.Query != b.Query || a.Observation != b.Observation || a.Error != b.Error || a.AgentRole != b.AgentRole || a.TokenUsage != b.TokenUsage {
		return false
	}
	if (a.OccurredAt == nil) != (b.OccurredAt == nil) || (a.DurationMS == nil) != (b.DurationMS == nil) || (a.Evidence == nil) != (b.Evidence == nil) || len(a.Evidence) != len(b.Evidence) {
		return false
	}
	if a.OccurredAt != nil && *a.OccurredAt != *b.OccurredAt && !reflect.DeepEqual(a.OccurredAt, b.OccurredAt) {
		return false
	}
	if a.DurationMS != nil && *a.DurationMS != *b.DurationMS {
		return false
	}
	for i, e := range a.Evidence {
		other := b.Evidence[i]
		if e.Path != other.Path || e.Query != other.Query || (e.Lines == nil) != (other.Lines == nil) || !slices.Equal(e.Lines, other.Lines) {
			return false
		}
	}
	return true
}
