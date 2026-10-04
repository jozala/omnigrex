package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jozala/omnigrex/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestOperationPreservesDetachedParentAndNeverRecordsErrorText(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	parent, cancel := context.WithCancel(context.Background())
	ctx, execution := telemetry.StartOperation(parent, telemetry.AgentTurnExecute)
	detached := telemetry.CopyContext(context.Background(), ctx)
	cancel()
	if detached.Err() != nil {
		t.Fatal("detached lifecycle inherited cancellation")
	}
	child, launch := telemetry.StartOperation(detached, telemetry.RuntimeProcessLaunch)
	telemetry.SetOutcome(child, telemetry.Failure, "operation_failed")
	err := errors.Join(errors.New("secret-provider-token"), context.DeadlineExceeded)
	launch.Finish(&err)
	var success error
	execution.Finish(&success)
	spans := recorder.Ended()
	if len(spans) != 2 || spans[0].Parent().SpanID() != trace.SpanContextFromContext(ctx).SpanID() {
		t.Fatalf("incorrect spans: %v", spans)
	}
	if spans[0].Status().Code != codes.Error || spans[0].Status().Description != "operation_failed" || len(spans[0].Events()) != 0 {
		t.Fatalf("unsafe failure: %v", spans[0])
	}
	if !trace.SpanContextFromContext(telemetry.ErrorContext(context.Background(), err)).Equal(trace.SpanContextFromContext(child)) {
		t.Fatal("callback error lost operation context")
	}
}
