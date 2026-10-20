//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type sdkCallDiagnostic struct {
	Operation string
	Resource  metadata
	Started   time.Time
}

func sdkCall(operation string, resource metadata) sdkCallDiagnostic {
	return sdkCallDiagnostic{Operation: operation, Resource: resource, Started: time.Now()}
}

func logSDKCallFailure(ctx context.Context, call sdkCallDiagnostic, kind string, err error, details ...slog.Attr) {
	if err == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	level := slog.LevelError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		level = slog.LevelWarn
	}
	attrs := resourceLogAttrs(call.Resource)
	attrs = append(attrs, details...)
	// TODO: Ensure Microsandbox SDK diagnostics do not log sensitive data; raw SDK errors are currently logged without redaction.
	attrs = append(attrs, slog.String("sdk_operation", call.Operation), slog.String("sdk_error_kind", kind), slog.String("error", err.Error()), slog.Int64("duration_ms", max(0, time.Since(call.Started).Milliseconds())))
	slog.Default().LogAttrs(ctx, level, "Microsandbox SDK call failed", attrs...)
}
