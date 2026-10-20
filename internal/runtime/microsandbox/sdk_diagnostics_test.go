//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestSDKCallDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		level string
	}{
		{"failure", errors.New("token=fake-secret\noriginal cause"), "ERROR"},
		{"cancelled", context.Canceled, "WARN"},
		{"deadline", context.DeadlineExceeded, "WARN"},
		{"success", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := captureDiagnostics(t)
			key := testRequest().Key
			identity, _ := json.Marshal(key)
			span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
			ctx := trace.ContextWithSpanContext(context.Background(), span)
			logSDKCallFailure(ctx, sdkCallDiagnostic{Operation: "GetSandbox", Started: time.Now(), Resource: metadata{ID: "native-id", Name: "sandbox", Status: "Stopped", Labels: map[string]string{identityLabel: string(identity)}}}, "Internal", test.err)
			if test.err == nil {
				if output.Len() != 0 {
					t.Fatal(output.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{"error": test.err.Error(), "level": test.level, "sdk_operation": "GetSandbox", "sdk_error_kind": "Internal", "resource_id": "native-id", "resource_name": "sandbox", "agent_participant_id": key.AssignmentID, "agent_session_id": key.AgentSessionID, "agent_turn_id": key.AgentTurnID, "execution_epoch": float64(1), "trace_id": span.TraceID().String(), "span_id": span.SpanID().String()} {
				if record[field] != want {
					t.Errorf("%s = %v, want %v", field, record[field], want)
				}
			}
			if record["duration_ms"].(float64) < 0 || strings.Count(output.String(), "\n") != 1 {
				t.Fatal(output.String())
			}
		})
	}
	t.Run("missing identity", func(t *testing.T) {
		output := captureDiagnostics(t)
		logSDKCallFailure(context.Background(), sdkCallDiagnostic{Operation: "ListSandboxes", Started: time.Now()}, "Unknown", errors.New("cause"))
		if strings.Contains(output.String(), "agent_participant_id") || strings.Contains(output.String(), "resource_id") {
			t.Fatal(output.String())
		}
	})
}
