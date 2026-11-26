//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"go.opentelemetry.io/otel/trace"
)

type shutdownReceive struct {
	started  chan struct{}
	result   func(context.Context) (*sdk.ExecEvent, error)
	released atomic.Int32
}

func (h *shutdownReceive) Recv(ctx context.Context) (*sdk.ExecEvent, error) {
	close(h.started)
	<-ctx.Done()
	return h.result(ctx)
}
func (h *shutdownReceive) Close() error { h.released.Add(1); return nil }

func TestTransportSDKShutdownDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  func(context.Context) (*sdk.ExecEvent, error)
		failure bool
	}{
		{"intentional close", func(ctx context.Context) (*sdk.ExecEvent, error) {
			return nil, &sdk.Error{Kind: sdk.ErrCancelled, Cause: ctx.Err()}
		}, false},
		{"internal failure racing close", func(context.Context) (*sdk.ExecEvent, error) {
			return nil, &sdk.Error{Kind: sdk.ErrInternal, Cause: context.Canceled}
		}, true},
		{"deadline racing close", func(context.Context) (*sdk.ExecEvent, error) {
			return nil, &sdk.Error{Kind: sdk.ErrCancelled, Cause: context.DeadlineExceeded}
		}, true},
		{"unattributed cancellation", func(context.Context) (*sdk.ExecEvent, error) { return nil, &sdk.Error{Kind: sdk.ErrCancelled} }, true},
		{"joined failure racing close", func(context.Context) (*sdk.ExecEvent, error) {
			return nil, errors.Join(&sdk.Error{Kind: sdk.ErrCancelled, Cause: context.Canceled}, errors.New("native failure"))
		}, true},
		{"failure event racing close", func(context.Context) (*sdk.ExecEvent, error) { return &sdk.ExecEvent{Kind: sdk.ExecEventFailed}, nil }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := captureDiagnostics(t)
			handle := &shutdownReceive{started: make(chan struct{}), result: test.result}
			key := testRequest().Key
			identity, _ := json.Marshal(key)
			span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
			var detached atomic.Int32
			session := &sdkStream{handle: handle, sandbox: &releaseTestSandbox{detach: func(context.Context) error { detached.Add(1); return nil }}, resource: metadata{ID: "shutdown-fixture", Labels: map[string]string{identityLabel: string(identity)}}, ctx: trace.ContextWithSpanContext(context.Background(), span)}
			transport := newTransport()
			if err := transport.attach(session, io.Discard, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handle.started:
			case <-time.After(time.Second):
				t.Fatal("receive not started")
			}
			for range 2 {
				if err := transport.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if handle.released.Load() != 1 || detached.Load() != 1 {
				t.Fatal("release not exactly once")
			}
			if !test.failure {
				if output.Len() != 0 {
					t.Fatal(output.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err, output.String())
			}
			for field, want := range map[string]any{"sdk_operation": "Exec.Recv", "agent_turn_id": key.AgentTurnID, "trace_id": span.TraceID().String()} {
				if record[field] != want {
					t.Fatalf("%s=%v, want %v", field, record[field], want)
				}
			}
			if !strings.Contains(output.String(), "Microsandbox SDK call failed") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestSDKReceiveCancellationWithoutLocalClose(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		output := captureDiagnostics(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if deadline {
			var stop context.CancelFunc
			ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer stop()
		}
		session := &sdkStream{handle: &failedDiagnosticExec{err: &sdk.Error{Kind: sdk.ErrCancelled, Cause: ctx.Err()}}}
		_, err := session.Next(ctx)
		if !errors.Is(err, ctx.Err()) || !strings.Contains(output.String(), "Microsandbox SDK call failed") {
			t.Fatal(err, output.String())
		}
	}
}

func TestTransportSDKUnexpectedCancellationWhileOpen(t *testing.T) {
	output := captureDiagnostics(t)
	handle := &shutdownReceive{started: make(chan struct{}), result: func(ctx context.Context) (*sdk.ExecEvent, error) {
		return nil, &sdk.Error{Kind: sdk.ErrCancelled, Message: "private SDK detail", Cause: ctx.Err()}
	}}
	session := &sdkStream{handle: handle, sandbox: &releaseTestSandbox{detach: func(context.Context) error { return nil }}}
	transport := newTransport()
	if err := transport.attach(session, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	<-handle.started
	// Cancellation alone is not local-close authority, even on the private lifetime.
	transport.cancel(context.Canceled)
	select {
	case <-transport.done:
	case <-time.After(time.Second):
		t.Fatal("receive not interrupted")
	}
	_, err := transport.Read(make([]byte, 1))
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private SDK detail") {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Microsandbox SDK call failed") {
		t.Fatal(output.String())
	}
}

func TestSDKExpectedReceiveCancellationKeepsSafeReturn(t *testing.T) {
	output := captureDiagnostics(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errTransportClosed)
	source := &sdk.Error{Kind: sdk.ErrCancelled, Message: "private SDK detail", Cause: context.Canceled}
	session := &sdkStream{handle: &failedDiagnosticExec{err: source}}
	_, err := session.Next(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, source) || strings.Contains(err.Error(), "private SDK detail") || output.Len() != 0 {
		t.Fatal(err, output.String())
	}
}
