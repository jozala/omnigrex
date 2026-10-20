//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"go.opentelemetry.io/otel/trace"
)

func TestSDKErrorLogsDetailsButReturnsSafeError(t *testing.T) {
	output := captureDiagnostics(t)
	source := &sdk.Error{Kind: sdk.ErrInternal, Message: "SDK operation rejected", Cause: errors.New("original SDK cause fake-api-key")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sdkError(ctx, sdkCall("Kill", metadata{ID: "native-id"}), source)
	if !strings.Contains(output.String(), source.Error()) || !strings.Contains(output.String(), "Kill") || !strings.Contains(output.String(), `"sdk_error_kind":"Internal"`) {
		t.Fatal(output.String())
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, source) || strings.Contains(err.Error(), "api-key") {
		t.Fatal(err)
	}
}

type failedDiagnosticExec struct{ err error }

func (handle *failedDiagnosticExec) Recv(context.Context) (*sdk.ExecEvent, error) {
	return nil, handle.err
}
func (*failedDiagnosticExec) Close() error { return nil }

func TestSDKStreamFailuresKeepExecutionContext(t *testing.T) {
	output := captureDiagnostics(t)
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	source := errors.New("native receive cause")
	transport := &sdkStream{handle: &failedDiagnosticExec{err: source}, resource: metadata{ID: "original"}, ctx: trace.ContextWithSpanContext(context.Background(), span)}
	_, err := transport.Next(context.Background())
	if err == nil || errors.Is(err, source) {
		t.Fatalf("unsafe receive error: %v", err)
	}
	if !strings.Contains(output.String(), span.TraceID().String()) || !strings.Contains(output.String(), "original") || !strings.Contains(output.String(), source.Error()) {
		t.Fatal(output.String())
	}
}

func TestSDKReleaseReportsExpiredBudget(t *testing.T) {
	output := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := sdkRelease("Exec.Close", metadata{ID: "original"}, func(context.Context) error { cancel(); return nil })
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "WARN") || !strings.Contains(output.String(), "Exec.Close") || !strings.Contains(output.String(), "context canceled") {
		t.Fatalf("late release lost expired-budget diagnostic: %s", output)
	}
}

func TestSDKExpectedAbsenceDoesNotLogFailure(t *testing.T) {
	output := captureDiagnostics(t)
	source := &sdk.Error{Kind: sdk.ErrSandboxNotFound}
	if !errors.Is(sdkLookupError(context.Background(), sdkCall("GetSandbox", metadata{}), source), execution.ErrNotFound) || output.Len() != 0 {
		t.Fatal(output.String())
	}
}
