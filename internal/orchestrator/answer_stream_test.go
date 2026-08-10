package orchestrator

import (
	"strings"
	"testing"
)

func TestFinalAnswerStreamOnlyEmitsFinalAnswer(t *testing.T) {
	var chunks []string
	stream := newFinalAnswerStream(func(chunk string) { chunks = append(chunks, chunk) })
	for _, chunk := range []string{
		`{"thought_summary":"secret reasoning and \"final_answer\" mention",`,
		`"stop":true,"final_`,
		`answer":"STREAM_`,
		`OK\n完成","actions":[{"action":"secret_tool"}]}`,
	} {
		stream.Write(chunk)
	}
	got := strings.Join(chunks, "")
	if got != "STREAM_OK\n完成" {
		t.Fatalf("streamed answer = %q; chunks = %#v", got, chunks)
	}
	if strings.Contains(got, "reasoning") || strings.Contains(got, "secret_tool") {
		t.Fatalf("internal planner content leaked: %q", got)
	}
}

func TestFinalAnswerStreamHandlesEscapedQuoteAcrossChunks(t *testing.T) {
	var output strings.Builder
	stream := newFinalAnswerStream(func(chunk string) { output.WriteString(chunk) })
	stream.Write(`{"final_answer":"say \`)
	stream.Write(`"hello\" now"}`)
	if got := output.String(); got != `say "hello" now` {
		t.Fatalf("streamed answer = %q", got)
	}
}
