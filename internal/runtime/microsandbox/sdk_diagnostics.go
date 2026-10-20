package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type sdkCallDiagnostic struct {
	Operation string
	Resource  metadata
	Started   time.Time
}

func sdkCall(operation string, resource metadata) sdkCallDiagnostic {
	return sdkCallDiagnostic{Operation: operation, Resource: resource, Started: time.Now()}
}

func resourceLogAttrs(resource metadata) []slog.Attr {
	var attrs []slog.Attr
	for key, value := range map[string]string{"resource_id": resource.ID, "resource_name": resource.Name, "resource_status": resource.Status} {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	var key execution.Key
	if json.Unmarshal([]byte(resource.Labels[identityLabel]), &key) == nil && validKey(key) {
		attrs = append(attrs, slog.String("participant_id", key.AssignmentID), slog.String("agent_session_id", key.AgentSessionID), slog.String("agent_turn_id", key.AgentTurnID), slog.Int64("execution_epoch", key.ExecutionEpoch))
	}
	return attrs
}

func logSDKCallFailure(ctx context.Context, call sdkCallDiagnostic, kind string, err error) {
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
	// TODO: Ensure Microsandbox SDK diagnostics do not log sensitive data; raw SDK errors are currently logged without redaction.
	attrs = append(attrs, slog.String("sdk_operation", call.Operation), slog.String("sdk_error_kind", kind), slog.String("error", err.Error()), slog.Int64("duration_ms", max(0, time.Since(call.Started).Milliseconds())))
	slog.Default().LogAttrs(ctx, level, "Microsandbox SDK call failed", attrs...)
}
