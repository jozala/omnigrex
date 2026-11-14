//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/environment"
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

type failureEventExec struct{ record *sdk.ExecEvent }

func (handle *failureEventExec) Recv(context.Context) (*sdk.ExecEvent, error) {
	return handle.record, nil
}
func (*failureEventExec) Close() error { return nil }

func TestSDKFailureEventsAreLoggedWithoutPublicDetails(t *testing.T) {
	for _, kind := range []sdk.ExecEventKind{sdk.ExecEventFailed, sdk.ExecEventStdinError} {
		for _, setup := range []bool{false, true} {
			output := captureDiagnostics(t)
			errno := 13
			handle := &failureEventExec{record: &sdk.ExecEvent{Kind: kind, Data: []byte("private event data"), Failure: &sdk.ExecFailure{Kind: "PermissionDenied", Message: "original event cause fake-api-key", Path: "/private/program", Errno: &errno, ErrnoName: "EACCES"}}}
			meta := metadata{ID: "original"}
			if setup {
				err := awaitSDKExecution(context.Background(), meta, handle)
				if !errors.Is(err, execution.ErrUnknownEffect) || strings.Contains(err.Error(), "api-key") {
					t.Fatalf("unsafe setup result: %v", err)
				}
			} else {
				transport := &sdkStream{handle: handle, resource: meta}
				event, err := transport.Next(context.Background())
				if err != nil || event.Kind != "failed" || len(event.Data) != 0 {
					t.Fatalf("unsafe stream result: %+v, %v", event, err)
				}
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal("missing event diagnostic", err)
			}
			if record["error"] != "original event cause fake-api-key" || record["resource_id"] != "original" || record["sdk_operation"] != "Exec.Recv" || record["sdk_failure_errno"] != float64(13) || record["sdk_failure_path"] != "/private/program" {
				t.Fatal(record)
			}
		}
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
	blocked := make(chan struct{})
	defer close(blocked)
	err := boundedNativeRelease(context.Background(), time.Millisecond, sdkRelease("Exec.Close", metadata{ID: "original"}, func(context.Context) error { <-blocked; return nil }))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "WARN") || !strings.Contains(output.String(), "Exec.Close") || !strings.Contains(output.String(), "release_wait") {
		t.Fatalf("late release lost expired-budget diagnostic: %s", output)
	}
}

func TestSDKReleasePreservesLateOriginalFailure(t *testing.T) {
	output := captureDiagnostics(t)
	blocked := make(chan struct{})
	source := errors.New("original late native close failure")
	err := boundedNativeRelease(context.Background(), time.Millisecond, sdkRelease("Exec.Close", metadata{ID: "original"}, func(context.Context) error { <-blocked; return source }))
	close(blocked)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, source) || strings.Contains(err.Error(), source.Error()) {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !strings.Contains(output.String(), source.Error()) {
		select {
		case <-output.notifications:
		case <-deadline.C:
			t.Fatal("late SDK error was suppressed", output.String())
		}
	}
	if !strings.Contains(output.String(), "release_wait") {
		t.Fatal("unresolved wait was not reported", output.String())
	}
}

func TestSDKReleaseTimeoutAlwaysHasDiagnostic(t *testing.T) {
	output := captureDiagnostics(t)
	for range 2000 {
		before := output.Len()
		err := boundedNativeRelease(context.Background(), time.Microsecond, sdkRelease("Detach", metadata{ID: "original"}, func(context.Context) error { return nil }))
		if errors.Is(err, context.DeadlineExceeded) && output.Len() == before {
			t.Fatal("timeout returned without an attributable diagnostic")
		}
	}
}

func TestSDKExpectedAbsenceDoesNotLogFailure(t *testing.T) {
	output := captureDiagnostics(t)
	source := &sdk.Error{Kind: sdk.ErrSandboxNotFound}
	if !errors.Is(sdkLookupError(context.Background(), sdkCall("GetSandbox", metadata{}), source), execution.ErrNotFound) || output.Len() != 0 {
		t.Fatal(output.String())
	}
}

type qualificationEvents struct {
	events []*sdk.ExecEvent
	err    error
}

func (handle *qualificationEvents) Recv(context.Context) (*sdk.ExecEvent, error) {
	if len(handle.events) == 0 {
		return nil, handle.err
	}
	event := handle.events[0]
	handle.events = handle.events[1:]
	return event, nil
}
func TestSDKQualificationCaptureRequiresObservedFailure(t *testing.T) {
	for _, terminal := range []sdk.ExecEventKind{sdk.ExecEventExited, sdk.ExecEventDone, sdk.ExecEventFailed} {
		output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
		handle := &qualificationEvents{events: []*sdk.ExecEvent{
			{Kind: sdk.ExecEventStdout, Data: []byte("stdout-secret")},
			{Kind: sdk.ExecEventStderr, Data: []byte("stderr-secret\nPi resource check failed: owned check")},
			{Kind: sdk.ExecEventStderr, Data: []byte("sum runner.mjs\n")},
			{Kind: terminal, ExitCode: 7},
		}}
		err := awaitSDKQualification(context.Background(), metadata{}, handle, output)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
		if strings.Contains(output.Check(), "owned checksum runner.mjs") != (terminal == sdk.ExecEventExited) {
			t.Fatal(output.Check(), err)
		}
		if terminal != sdk.ExecEventExited && !errors.Is(err, execution.ErrUnknownEffect) {
			t.Fatal(err)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if cause == context.DeadlineExceeded {
			var stop context.CancelFunc
			ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer stop()
		}
		output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
		handle := &qualificationEvents{events: []*sdk.ExecEvent{{Kind: sdk.ExecEventStderr, Data: []byte("Pi resource check failed: Node version\n")}}, err: cause}
		err := awaitSDKQualification(ctx, metadata{}, handle, output)
		if !errors.Is(err, cause) || !strings.Contains(output.Check(), "unknown stage") {
			t.Fatal(err, output.Check())
		}
	}
	handle := &qualificationEvents{events: []*sdk.ExecEvent{{Kind: sdk.ExecEventStderr, Data: []byte("private guest output")}, {Kind: sdk.ExecEventExited, ExitCode: 1}}}
	if err := awaitSDKExecution(context.Background(), metadata{}, handle); err == nil || err.Error() != "Microsandbox guest setup failed" {
		t.Fatal(err)
	}
}
