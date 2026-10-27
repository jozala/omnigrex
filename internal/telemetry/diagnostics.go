package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"syscall"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// inspectError never calls Error, Is, or As: those methods can contain remote
// text or arbitrary extraction code. Bound traversal also handles cyclic wraps.
func inspectError(err error, visit func(error) bool) {
	pending := []error{err}
	for budget := 0; len(pending) > 0 && budget < 32; budget++ {
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if e == nil {
			continue
		}
		if visit(e) {
			return
		}
		switch e := e.(type) {
		case interface{ Unwrap() error }:
			pending = append(pending, e.Unwrap())
		case interface{ Unwrap() []error }:
			children := e.Unwrap()
			n := min(len(children), 32-len(pending))
			if n > 0 {
				pending = append(pending, children[:n]...)
			}
		}
	}
}

type failureDiagnostic struct {
	signal, stage, kind string
	rejected            *int64
}

func diagnose(err error, signal, stage string) (d failureDiagnostic) {
	d = failureDiagnostic{signal: signal, stage: stage, kind: "unknown"}
	// A broken error implementation must not turn diagnostics into a panic or
	// trigger an unsafe fallback to its original text.
	defer func() {
		if recover() != nil {
			d = failureDiagnostic{signal: signal, stage: stage, kind: "unknown"}
		}
	}()
	inspectError(err, func(e error) bool {
		if ps, ok := partialSuccess(e); ok {
			d = ps
			return true
		}
		switch e := e.(type) {
		case syscall.Errno:
			switch e {
			case syscall.ECONNRESET:
				d.kind = "connection_reset"
			case syscall.ECONNREFUSED:
				d.kind = "connection_refused"
			case syscall.ETIMEDOUT:
				d.kind = "timeout"
			}
		case net.Error:
			if e.Timeout() {
				d.kind = "timeout"
			}
		}
		// Equality is restricted to comparable concrete types; arbitrary error
		// values can hold slices and must not be compared as interfaces.
		if reflect.TypeOf(e).Comparable() {
			switch e {
			case io.EOF:
				d.kind = "eof"
			case io.ErrUnexpectedEOF:
				d.kind = "unexpected_eof"
			case context.Canceled:
				d.kind = "cancelled"
			case context.DeadlineExceeded:
				d.kind = "timeout"
			}
		}
		return false
	})
	return d
}

// v1.35.0 exposes PartialSuccess only from an internal package. Inspect only
// the exact pinned SDK type, never similarly named user types or error text.
// Missing/changed fields on upgrade safely degrade to unknown.
func partialSuccess(err error) (failureDiagnostic, bool) {
	t := reflect.TypeOf(err)
	if t == nil || t.Name() != "PartialSuccess" {
		return failureDiagnostic{}, false
	}
	signal := ""
	switch t.PkgPath() {
	case "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp/internal":
		signal = "metrics"
	case "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp/internal":
		signal = "traces"
	default:
		return failureDiagnostic{}, false
	}
	v := reflect.ValueOf(err)
	if v.Kind() != reflect.Struct {
		return failureDiagnostic{}, false
	}
	n := v.FieldByName("RejectedItems")
	if !n.IsValid() || n.Kind() != reflect.Int64 || n.Int() < 0 {
		return failureDiagnostic{}, false
	}
	count := n.Int()
	return failureDiagnostic{signal: signal, stage: "response", kind: "partial_success", rejected: &count}, true
}

func logTelemetryFailure(err error, signal, stage string) {
	if err == nil {
		return
	}
	d := diagnose(err, signal, stage)
	attrs := []any{"signal", d.signal, "stage", d.stage, "error_kind", d.kind}
	if d.rejected != nil {
		attrs = append(attrs, "rejected_items", *d.rejected)
	}
	// Ordinary stdout logging only: never create spans or record metrics here.
	slog.Warn("telemetry SDK failure", attrs...)
}

type diagnosticMetricExporter struct{ sdkmetric.Exporter }

func (e diagnosticMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, data)
	logTelemetryFailure(err, "metrics", "export")
	return err
}

type diagnosticTraceExporter struct{ sdktrace.SpanExporter }

func (e diagnosticTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	logTelemetryFailure(err, "traces", "export")
	return err
}
