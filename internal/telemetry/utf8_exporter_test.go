package telemetry

import (
	"context"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type captureExporter struct {
	spans []sdktrace.ReadOnlySpan
}

func (e *captureExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *captureExporter) Shutdown(context.Context) error { return nil }

func TestUTF8SanitizingExporterCleansSpanStrings(t *testing.T) {
	bad := "bad\xffvalue"
	stub := tracetest.SpanStub{
		Name:       bad,
		Attributes: []attribute.KeyValue{attribute.String("field", bad), attribute.StringSlice("fields", []string{"ok", bad})},
		Events: []sdktrace.Event{{
			Name:       bad,
			Attributes: []attribute.KeyValue{attribute.String("exception.message", bad)},
		}},
		Status:   sdktrace.Status{Code: codes.Error, Description: bad},
		Resource: resource.NewWithAttributes("", attribute.String("resource.field", bad)),
	}
	capture := &captureExporter{}
	exporter := newUTF8SanitizingExporter(capture)

	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	if len(capture.spans) != 1 {
		t.Fatalf("captured spans = %d, want 1", len(capture.spans))
	}
	span := capture.spans[0]
	assertValidUTF8(t, span.Name())
	assertValidUTF8(t, span.Status().Description)
	assertAttributesValidUTF8(t, span.Attributes())
	assertAttributesValidUTF8(t, span.Resource().Attributes())
	for _, event := range span.Events() {
		assertValidUTF8(t, event.Name)
		assertAttributesValidUTF8(t, event.Attributes)
	}
}

func assertAttributesValidUTF8(t *testing.T, attrs []attribute.KeyValue) {
	t.Helper()
	for _, kv := range attrs {
		assertValidUTF8(t, string(kv.Key))
		switch kv.Value.Type() {
		case attribute.STRING:
			assertValidUTF8(t, kv.Value.AsString())
		case attribute.STRINGSLICE:
			for _, value := range kv.Value.AsStringSlice() {
				assertValidUTF8(t, value)
			}
		}
	}
}

func assertValidUTF8(t *testing.T, value string) {
	t.Helper()
	if !utf8.ValidString(value) {
		t.Fatalf("invalid UTF-8 remains in %q", value)
	}
}
