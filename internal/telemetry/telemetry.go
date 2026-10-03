// Package telemetry configures optional OTLP/HTTP tracing for the orchestrator.
package telemetry

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init installs the global trace provider when an OTLP endpoint is configured.
// Exporter, resource, and sampler settings use standard OTEL environment variables.
// The returned shutdown function must be called after all services have stopped.
func Init(ctx context.Context, version string) (func(context.Context) error, error) {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") ||
		(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "") {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("omnigrex"), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return provider.Shutdown, nil
}
