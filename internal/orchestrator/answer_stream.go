package orchestrator

import (
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
)

// finalAnswerStream incrementally extracts only the JSON final_answer string
// from a structured planner response. Planner thoughts and tool parameters
// must never be forwarded to user-facing SSE token events.
type finalAnswerStream struct {
	stream *llmcore.JSONStringFieldStream
}

func newFinalAnswerStream(emit func(string)) *finalAnswerStream {
	return &finalAnswerStream{stream: llmcore.NewJSONStringFieldStream("final_answer", emit)}
}

func (s *finalAnswerStream) Write(chunk string) {
	if s != nil && s.stream != nil {
		s.stream.Write(chunk)
	}
}
