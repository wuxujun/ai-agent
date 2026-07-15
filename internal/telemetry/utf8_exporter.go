package telemetry

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// utf8SanitizingExporter keeps malformed external strings from causing an
// OTLP encoder to reject an entire batch of otherwise valid spans.
type utf8SanitizingExporter struct {
	next sdktrace.SpanExporter
}

func newUTF8SanitizingExporter(next sdktrace.SpanExporter) sdktrace.SpanExporter {
	return &utf8SanitizingExporter{next: next}
}

func (e *utf8SanitizingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	sanitized := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		stub := tracetest.SpanStubFromReadOnlySpan(span)
		stub.Name = validUTF8(stub.Name)
		stub.Attributes = validUTF8Attributes(stub.Attributes)
		stub.Status.Description = validUTF8(stub.Status.Description)
		for j := range stub.Events {
			stub.Events[j].Name = validUTF8(stub.Events[j].Name)
			stub.Events[j].Attributes = validUTF8Attributes(stub.Events[j].Attributes)
		}
		for j := range stub.Links {
			stub.Links[j].Attributes = validUTF8Attributes(stub.Links[j].Attributes)
		}
		stub.InstrumentationScope = validUTF8Scope(stub.InstrumentationScope)
		stub.InstrumentationLibrary = validUTF8Scope(stub.InstrumentationLibrary)
		if stub.Resource != nil {
			stub.Resource = resource.NewWithAttributes(
				validUTF8(stub.Resource.SchemaURL()),
				validUTF8Attributes(stub.Resource.Attributes())...,
			)
		}
		sanitized[i] = stub.Snapshot()
	}
	return e.next.ExportSpans(ctx, sanitized)
}

func (e *utf8SanitizingExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}

func validUTF8(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, "\uFFFD")
}

func validUTF8Scope(scope instrumentation.Scope) instrumentation.Scope {
	scope.Name = validUTF8(scope.Name)
	scope.Version = validUTF8(scope.Version)
	scope.SchemaURL = validUTF8(scope.SchemaURL)
	scope.Attributes = attribute.NewSet(validUTF8Attributes(scope.Attributes.ToSlice())...)
	return scope
}

func validUTF8Attributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	if len(attrs) == 0 {
		return attrs
	}
	out := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		out[i] = kv
		out[i].Key = attribute.Key(validUTF8(string(kv.Key)))
		switch kv.Value.Type() {
		case attribute.STRING:
			out[i].Value = attribute.StringValue(validUTF8(kv.Value.AsString()))
		case attribute.STRINGSLICE:
			values := kv.Value.AsStringSlice()
			clean := make([]string, len(values))
			for j, value := range values {
				clean[j] = validUTF8(value)
			}
			out[i].Value = attribute.StringSliceValue(clean)
		}
	}
	return out
}
