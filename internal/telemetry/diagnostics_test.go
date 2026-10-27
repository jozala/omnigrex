package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

const diagnosticSecret = "SENTINEL-PRIVATE-TELEMETRY"

func captureDiagnosticLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestDiagnosticKinds(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{io.EOF, "eof"}, {io.ErrUnexpectedEOF, "unexpected_eof"},
		{syscall.ECONNRESET, "connection_reset"}, {syscall.ECONNREFUSED, "connection_refused"},
		{context.DeadlineExceeded, "timeout"}, {context.Canceled, "cancelled"},
		{errors.New(diagnosticSecret), "unknown"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			logs := captureDiagnosticLogs(t)
			err := fmt.Errorf("%s: %w", diagnosticSecret, &url.Error{Op: diagnosticSecret, URL: "https://" + diagnosticSecret + "/private?token=" + diagnosticSecret, Err: tc.err})
			logTelemetryFailure(err, "metrics", "export")
			if !strings.Contains(logs.String(), `"error_kind":"`+tc.kind+`"`) || strings.Contains(logs.String(), diagnosticSecret) {
				t.Fatalf("unsafe or incorrect diagnostic: %s", logs)
			}
		})
	}
}

type brokenDiagnosticError struct{}

func (brokenDiagnosticError) Error() string { panic("must not format errors") }
func (brokenDiagnosticError) Unwrap() error { panic(diagnosticSecret) }
func TestDiagnosticExtractionFallback(t *testing.T) {
	d := diagnose(brokenDiagnosticError{}, "unknown", "unknown")
	if d.kind != "unknown" || d.signal != "unknown" || d.stage != "unknown" {
		t.Fatalf("unexpected fallback: %+v", d)
	}
}

func TestOTLPDiagnosticFixtures(t *testing.T) {
	for _, signal := range []string{"metrics", "traces"} {
		for _, scenario := range []string{"disconnect", "rejection", "partial", "invalid_response"} {
			t.Run(signal+"/"+scenario, func(t *testing.T) {
				cleanEnvironment(t)
				logs := captureDiagnosticLogs(t)
				otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { logTelemetryFailure(err, "unknown", "unknown") }))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != diagnosticSecret {
						t.Error("fixture header missing")
					}
					_, _ = io.Copy(io.Discard, r.Body)
					switch scenario {
					case "disconnect":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					case "rejection":
						w.Header().Set("X-Private", diagnosticSecret)
						w.WriteHeader(400)
						_, _ = io.WriteString(w, diagnosticSecret)
					case "invalid_response":
						w.Header().Set("Content-Type", "application/x-protobuf")
						_, _ = io.WriteString(w, diagnosticSecret)
					case "partial":
						w.Header().Set("Content-Type", "application/x-protobuf")
						var message proto.Message = &metrics.ExportMetricsServiceResponse{PartialSuccess: &metrics.ExportMetricsPartialSuccess{RejectedDataPoints: 7, ErrorMessage: diagnosticSecret}}
						if signal == "traces" {
							message = &traces.ExportTraceServiceResponse{PartialSuccess: &traces.ExportTracePartialSuccess{RejectedSpans: 7, ErrorMessage: diagnosticSecret}}
						}
						data, err := proto.Marshal(message)
						if err != nil {
							t.Error(err)
							return
						}
						_, _ = w.Write(data)
					}
				}))
				defer server.Close()
				endpoint := server.URL + "/" + diagnosticSecret
				var exportErr error
				if signal == "metrics" {
					raw, err := otlpmetrichttp.New(context.Background(), otlpmetrichttp.WithEndpointURL(endpoint), otlpmetrichttp.WithHeaders(map[string]string{"Authorization": diagnosticSecret}))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = raw.Shutdown(context.Background()) }()
					exportErr = (diagnosticMetricExporter{raw}).Export(context.Background(), &metricdata.ResourceMetrics{})
				} else {
					raw, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithHeaders(map[string]string{"Authorization": diagnosticSecret}))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = raw.Shutdown(context.Background()) }()
					exportErr = (diagnosticTraceExporter{raw}).ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{tracetest.SpanStub{Name: diagnosticSecret}.Snapshot()})
				}
				kind, stage := "unknown", "export"
				switch scenario {
				case "disconnect":
					kind = "eof"
				case "partial":
					kind = "partial_success"
					stage = "response"
				}
				if (scenario == "partial") != (exportErr == nil) {
					t.Fatalf("export error behavior changed: %v", exportErr != nil)
				}
				text := logs.String()
				for _, field := range []string{`"signal":"` + signal + `"`, `"stage":"` + stage + `"`, `"error_kind":"` + kind + `"`} {
					if !strings.Contains(text, field) {
						t.Fatalf("missing %s: %s", field, text)
					}
				}
				if scenario == "partial" && !strings.Contains(text, `"rejected_items":7`) {
					t.Fatalf("missing rejected count: %s", text)
				}
				if strings.Contains(text, diagnosticSecret) {
					t.Fatalf("secret leaked: %s", text)
				}
			})
		}
	}
}

func TestGlobalSDKFailureIsNotAnExportFailure(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	logTelemetryFailure(fmt.Errorf("callback failed: %s", diagnosticSecret), "unknown", "unknown")
	text := logs.String()
	if !strings.Contains(text, `"stage":"unknown"`) || !strings.Contains(text, `"signal":"unknown"`) || strings.Contains(text, "export") || strings.Contains(text, diagnosticSecret) {
		t.Fatalf("incorrect global fallback: %s", text)
	}
}

type failingMetricExporter struct {
	err   error
	calls int
}

func (e *failingMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	e.calls++
	return e.err
}

func (*failingMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}
func (*failingMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}
func (*failingMetricExporter) ForceFlush(context.Context) error { return nil }
func (*failingMetricExporter) Shutdown(context.Context) error   { return nil }

func TestExportDiagnosticsPreserveErrors(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		logs := captureDiagnosticLogs(t)
		raw := &failingMetricExporter{err: err}
		got := (diagnosticMetricExporter{raw}).Export(context.Background(), &metricdata.ResourceMetrics{})
		if got != err || raw.calls != 1 {
			t.Fatal("diagnostics changed export propagation or retried")
		}
		if !strings.Contains(logs.String(), `"signal":"metrics"`) || !strings.Contains(logs.String(), `"stage":"export"`) {
			t.Fatalf("missing export context: %s", logs)
		}
	}
}

func TestSDKCollectionFailureBeforeExportStaysUnknown(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	// A real SDK callback failure must not pass through the exporter boundary.
	// This callback is not the application's instrumented durable observer, so
	// neither signal nor stage can be recovered from its global SDK error.
	raw := &failingMetricExporter{}
	reader := sdkmetric.NewPeriodicReader(diagnosticMetricExporter{raw}, sdkmetric.WithInterval(time.Hour))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	meter := provider.Meter("fixture")
	gauge, err := meter.Int64ObservableGauge("fixture")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := meter.RegisterCallback(func(context.Context, metric.Observer) error { return fmt.Errorf("%s: %w", diagnosticSecret, io.EOF) }, gauge)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registration.Unregister() }()
	err = provider.ForceFlush(context.Background())
	if err == nil {
		t.Fatal("expected callback failure")
	}
	logTelemetryFailure(err, "unknown", "unknown")
	text := logs.String()
	if raw.calls != 0 || strings.Contains(text, "export") || strings.Contains(text, diagnosticSecret) || !strings.Contains(text, `"error_kind":"eof"`) || !strings.Contains(text, `"stage":"unknown"`) || !strings.Contains(text, `"signal":"unknown"`) {
		t.Fatalf("collection was misclassified: %s", text)
	}
}
