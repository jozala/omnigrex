package agentevent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

func TestAgentEventCarriesExecutionContextAndOperationalMetadata(t *testing.T) {
	observed := time.Date(2026, 9, 3, 10, 11, 12, 0, time.UTC)
	event := agentevent.AgentEvent{
		AssignmentID:    "assignment-1",
		AgentSessionID:  "agent-session-1",
		TurnID:          "turn-1",
		ExecutionEpoch:  9,
		ControlRevision: 12,
		Kind:            agentevent.Kind("tool_call_update"),
		ObservedAt:      observed,
		Metadata: agentevent.OperationalMetadata{
			ToolCallID: "tool-1",
			ToolName:   "bash",
			Status:     "completed",
			Runtime:    json.RawMessage(`{"retry":false}`),
		},
	}

	if event.AssignmentID != "assignment-1" || event.AgentSessionID != "agent-session-1" || event.TurnID != "turn-1" {
		t.Fatalf("AgentEvent identifiers = %#v", event)
	}
	if event.ExecutionEpoch != 9 || event.ControlRevision != 12 || !event.ObservedAt.Equal(observed) {
		t.Fatalf("AgentEvent execution context = %#v", event)
	}
	if event.Metadata.ToolCallID != "tool-1" || event.Metadata.ToolName != "bash" || event.Metadata.Status != "completed" {
		t.Fatalf("AgentEvent metadata = %#v", event.Metadata)
	}
}

func TestNoopSinkAcceptsEvent(t *testing.T) {
	var sink agentevent.Sink = agentevent.NoopSink{}
	if err := sink.Emit(context.Background(), agentevent.AgentEvent{}); err != nil {
		t.Fatalf("NoopSink.Emit() error = %v", err)
	}
}
