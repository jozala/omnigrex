// Package telemetry configures optional OTLP/HTTP application telemetry.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"golang.org/x/net/http/httpguts"
)

// Init preserves endpoint-based tracing and separately opts application metrics in.
// Exporter, resource, and sampler settings use standard OTEL environment variables.
// The returned shutdown function must be called after all services have stopped.
func Init(ctx context.Context, version string) (func(context.Context) error, error) {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return func(context.Context) error { return nil }, nil
	}
	traces := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	metrics, err := metricsEnabled()
	if err != nil {
		return nil, err
	}
	if !traces && !metrics {
		return func(context.Context) error { return nil }, nil
	}
	if err := validateExporterSettings(traces, metrics); err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("omnigrex"), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, errors.New("initialize telemetry resource")
	}
	var traceProvider *sdktrace.TracerProvider
	var meterProvider *sdkmetric.MeterProvider
	if metrics {
		for _, name := range []string{"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_METRIC_EXPORT_TIMEOUT", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT"} {
			if value := os.Getenv(name); value != "" {
				milliseconds, err := strconv.ParseInt(value, 10, 64)
				if err != nil || milliseconds <= 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
					return nil, errors.New("invalid metric export timing")
				}
			}
		}
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, errors.New("initialize OTLP/HTTP metrics exporter")
		}
		// The SDK default is 60 seconds; standard interval/timeout overrides apply.
		meterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)), sdkmetric.WithView(applicationMetricView))
	}
	if traces {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			if meterProvider != nil {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = meterProvider.Shutdown(cleanup)
				cancel()
			}
			return nil, errors.New("initialize OTLP/HTTP trace exporter")
		}
		traceProvider = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
		otel.SetTracerProvider(traceProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	}
	if meterProvider != nil {
		otel.SetMeterProvider(meterProvider)
		metricsActive.Store(true)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		slog.Warn("telemetry delivery failed", "failure_category", "export_failed")
	}))
	return func(ctx context.Context) error {
		// Give both providers the same fresh bounded budget, concurrently, so an
		// unavailable signal cannot consume the other signal's entire shutdown.
		results := make(chan error, 2)
		go func() {
			if traceProvider != nil {
				results <- traceProvider.Shutdown(ctx)
			} else {
				results <- nil
			}
		}()
		go func() {
			if meterProvider != nil {
				results <- meterProvider.Shutdown(ctx)
			} else {
				results <- nil
			}
		}()
		err := errors.Join(<-results, <-results)
		if meterProvider != nil {
			metricsActive.Store(false)
		}
		return err
	}, nil
}

// The SDK parses generic settings before signal overrides and logs malformed
// header input verbatim. Validate every setting it will read before construction,
// including overridden values, and return only fixed, credential-free errors.
func validateExporterSettings(traces, metrics bool) error {
	prefixes := []string{"OTEL_EXPORTER_OTLP_"}
	if traces {
		prefixes = append(prefixes, "OTEL_EXPORTER_OTLP_TRACES_")
	}
	if metrics {
		prefixes = append(prefixes, "OTEL_EXPORTER_OTLP_METRICS_")
	}
	for _, prefix := range prefixes {
		if endpoint := os.Getenv(prefix + "ENDPOINT"); endpoint != "" {
			parsed, err := url.Parse(endpoint)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
				return errors.New("invalid OTLP/HTTP endpoint; use HTTP(S) and header authentication")
			}
		}
		if headers := os.Getenv(prefix + "HEADERS"); headers != "" {
			for _, pair := range strings.Split(headers, ",") {
				key, encoded, found := strings.Cut(pair, "=")
				value, err := url.PathUnescape(encoded)
				if !found || !httpguts.ValidHeaderFieldName(strings.TrimSpace(key)) || err != nil || !httpguts.ValidHeaderFieldValue(value) {
					return errors.New("invalid OTLP header configuration")
				}
			}
		}
	}
	return nil
}

func metricsEnabled() (bool, error) {
	switch os.Getenv("OTEL_METRICS_EXPORTER") {
	case "none":
		return false, nil
	case "":
		return os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "", nil
	case "otlp":
		if os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
			return false, errors.New("metrics export requires an OTLP endpoint")
		}
		return true, nil
	default:
		return false, errors.New("unsupported metrics exporter; use otlp or none")
	}
}
