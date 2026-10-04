package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/jozala/omnigrex/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

func TestLogCorrelationPreservesHandlerBehavior(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})))
	span := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2},
	}) // Valid unsampled context must also correlate.
	ctx := trace.ContextWithSpanContext(context.Background(), span)
	logger = logger.With("service", "test").WithGroup("details").With("retained", true)
	logger.DebugContext(ctx, "filtered")
	logger.InfoContext(ctx, "correlated", "value", 42)
	logger.Info("ordinary", "value", 7)
	decoder := json.NewDecoder(&output)
	var correlated, ordinary map[string]any
	if err := decoder.Decode(&correlated); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&ordinary); err != nil {
		t.Fatal(err)
	}
	if correlated["service"] != "test" || correlated["level"] != "INFO" || correlated["msg"] != "correlated" {
		t.Fatalf("handler fields changed: %v", correlated)
	}
	details := correlated["details"].(map[string]any)
	if details["trace_id"] != span.TraceID().String() || details["span_id"] != span.SpanID().String() {
		t.Fatalf("missing correlation: %v", correlated)
	}
	if details["retained"] != true || details["value"] != float64(42) {
		t.Fatalf("group lost: %v", details)
	}
	if _, ok := ordinary["details"].(map[string]any)["trace_id"]; ok {
		t.Fatalf("fabricated correlation: %v", ordinary)
	}
	if ordinary["msg"] != "ordinary" {
		t.Fatalf("level filtering failed: %v", ordinary)
	}
}

func TestCorrelationDoesNotRebindExistingAttributes(t *testing.T) {
	var output bytes.Buffer
	value := struct{ Value int }{Value: 1}
	boundCalls := 0
	handler := slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		if attr.Key == "bound" {
			boundCalls++
		}
		return attr
	}})
	logger := slog.New(telemetry.NewLogHandler(handler)).With("bound", &value)
	value.Value = 2
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}}))
	logger.InfoContext(ctx, "first")
	logger.InfoContext(ctx, "second")
	if boundCalls != 1 {
		t.Fatalf("bound attributes processed %d times", boundCalls)
	}
	decoder := json.NewDecoder(&output)
	for range 2 {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["bound"].(map[string]any)["Value"] != float64(1) {
			t.Fatal("bound value changed after WithAttrs")
		}
	}
}
