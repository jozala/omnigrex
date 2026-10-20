//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func unobservedRelease(run func(context.Context) error) nativeRelease { return nativeRelease{run: run} }

func TestNativeReleaseBoundsBlockingCloseAndStillDetaches(t *testing.T) {
	blocked := make(chan struct{})
	finished := make(chan struct{})
	detached := make(chan struct{})
	defer close(blocked)
	start := time.Now()
	err := boundedNativeRelease(context.Background(), 25*time.Millisecond,
		unobservedRelease(func(context.Context) error { <-blocked; close(finished); return nil }),
		unobservedRelease(func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("no deadline")
			}
			close(detached)
			return nil
		}),
	)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("blocking close reported settled release: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("release wait was unbounded")
	}
	select {
	case <-detached:
	default:
		t.Fatal("blocked exec close prevented detachment")
	}
	select {
	case <-finished:
		t.Fatal("timeout fabricated native completion")
	default:
	}
}

type releaseTestExec struct {
	close func() error
}

func (*releaseTestExec) Recv(context.Context) (*sdk.ExecEvent, error) {
	return &sdk.ExecEvent{Kind: sdk.ExecEventDone}, nil
}

func (handle *releaseTestExec) Close() error { return handle.close() }

type releaseTestSandbox struct {
	detach func(context.Context) error
}

func (sandbox *releaseTestSandbox) Detach(ctx context.Context) error { return sandbox.detach(ctx) }

func TestSDKStreamClosePreservesFailureAndDetachesExactlyOnce(t *testing.T) {
	output := captureDiagnostics(t)
	var closed, detached atomic.Int32
	source := errors.New("guest-output provider-api-key")
	transport := &sdkStream{
		handle: &releaseTestExec{close: func() error { closed.Add(1); return source }},
		sandbox: &releaseTestSandbox{detach: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("missing detach deadline")
			}
			detached.Add(1)
			return nil
		}},
	}
	first := transport.Close()
	second := transport.Close()
	if !strings.Contains(output.String(), source.Error()) {
		t.Fatalf("missing raw release diagnostics: %s", output)
	}
	if !errors.Is(first, execution.ErrUnknownEffect) || first != second || errors.Is(first, source) || strings.Contains(fmt.Sprintf("%+v", first), "api-key") {
		t.Fatalf("stream closure discarded failure or retained source: %v / %v", first, second)
	}
	if closed.Load() != 1 || detached.Load() != 1 {
		t.Fatalf("release repeated or detachment skipped: close=%d, detach=%d", closed.Load(), detached.Load())
	}
}

func TestSDKStreamCloseBoundsBackgroundNativeClose(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	detached := make(chan struct{})
	transport := &sdkStream{
		handle:  &releaseTestExec{close: func() error { <-blocked; return nil }},
		sandbox: &releaseTestSandbox{detach: func(context.Context) error { close(detached); return nil }},
	}
	result := make(chan error, 1)
	go func() { result <- transport.Close() }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownEffect) {
			t.Fatalf("blocked stream close claimed settled release: %v", err)
		}
	case <-time.After(nativeReleaseWait + 2*time.Second):
		t.Fatal("stream close blocked beyond its finite native-release wait")
	}
	select {
	case <-detached:
	default:
		t.Fatal("blocked stream close prevented detachment")
	}
}

func TestNativeReleaseBoundsDetachThatIgnoresCancellation(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	err := boundedNativeRelease(context.Background(), 25*time.Millisecond, unobservedRelease(func(context.Context) error { <-blocked; return nil }))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("blocking detach was not bounded/ambiguous: %v", err)
	}
}

func TestNativeReleaseAfterCallerCancellationGetsIndependentDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := boundedNativeRelease(ctx, time.Second, unobservedRelease(func(releaseCtx context.Context) error {
		if releaseCtx.Err() != nil {
			return errors.New("inherited caller cancellation")
		}
		if _, ok := releaseCtx.Deadline(); !ok {
			return errors.New("missing release deadline")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeReleasePreservesFailuresWithoutRetainingSource(t *testing.T) {
	source := errors.New("guest-output config provider-api-key")
	err := boundedNativeRelease(context.Background(), time.Second,
		unobservedRelease(func(context.Context) error { return source }),
		unobservedRelease(func(context.Context) error { return nil }),
	)
	if !errors.Is(err, execution.ErrUnknownEffect) || errors.Is(err, source) || strings.Contains(fmt.Sprintf("%+v", err), "api-key") {
		t.Fatalf("release failure discarded or source retained: %v", err)
	}
	operationFailure := context.Canceled
	if combined := errors.Join(operationFailure, err); !errors.Is(combined, context.Canceled) || !errors.Is(combined, execution.ErrUnknownEffect) {
		t.Fatalf("cleanup masked operation failure: %v", combined)
	}
}
