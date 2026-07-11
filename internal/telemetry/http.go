package telemetry

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// NewHTTPClient returns an http.Client with outbound OpenTelemetry tracing enabled.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return InstrumentHTTPClient(&http.Client{Timeout: timeout})
}

// InstrumentHTTPClient preserves the client's existing settings while wrapping
// its transport so outbound requests join the current trace context.
func InstrumentHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = otelhttp.NewTransport(base)
	return &clone
}
