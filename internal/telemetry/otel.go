package telemetry

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	otlpmetrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otlptracehttp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type ShutdownFunc func(context.Context) error

var NoopShutdown ShutdownFunc = func(context.Context) error { return nil }

func InitOTel(serviceName, env, endpoint string) (ShutdownFunc, error) {
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("deployment.environment", env),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	var traceExp sdktrace.SpanExporter
	var meterProvider *sdkmetric.MeterProvider

	// Support stdout exporter via endpoint parameter or OTEL_EXPORTER environment variable
	isStdout := endpoint == "stdout" || os.Getenv("OTEL_EXPORTER") == "stdout"

	if isStdout {
		traceExp, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("create stdout trace exporter: %w", err)
		}
		metricExp, err := stdoutmetric.New(stdoutmetric.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("create stdout metric exporter: %w", err)
		}
		meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second)),
			),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(meterProvider)
	} else {
		metricOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithInsecure()}
		traceOpts := []otlptracehttp.Option{otlptracehttp.WithInsecure()}
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			metricOpts = append(metricOpts, otlpmetrichttp.WithEndpointURL(endpoint))
			traceOpts = append(traceOpts, otlptracehttp.WithEndpointURL(endpoint))
		} else {
			metricOpts = append(metricOpts, otlpmetrichttp.WithEndpoint(endpoint))
			traceOpts = append(traceOpts, otlptracehttp.WithEndpoint(endpoint))
		}

		metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp metric exporter: %w", err)
		}

		meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second)),
			),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(meterProvider)

		traceExp, err = otlptracehttp.New(ctx, traceOpts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp trace exporter: %w", err)
		}
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("[OTel Error] telemetry connection/export error: %v", err)
	}))

	return func(ctx context.Context) error {
		if meterProvider != nil {
			if err := meterProvider.Shutdown(ctx); err != nil {
				return err
			}
		}
		return tracerProvider.Shutdown(ctx)
	}, nil
}
