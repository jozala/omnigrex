package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

func TestEmitAgentEventNormalizesSessionUpdateWithoutContent(t *testing.T) {
	const secret = "transcript-and-reasoning-sentinel"
	sink := &capturingAgentEventSink{}
	observed := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	err := acp.EmitAgentEvent(context.Background(), sink, agentevent.Context{
		AssignmentID:    "assignment-1",
		AgentSessionID:  "agent-session-uuid",
		ACPSessionID:    "opaque-acp-session-id",
		TurnID:          "turn-1",
		ExecutionEpoch:  4,
		ControlRevision: 7,
	}, observed, acp.SessionUpdate{
		SessionID: "opaque-acp-session-id",
		Update: json.RawMessage(`{
			"sessionUpdate":"tool_call_update",
			"messageId":"message-1",
			"toolCallId":"tool-1",
			"toolName":"bash",
			"kind":"execute",
			"status":"completed",
			"title":"` + secret + `",
			"content":{"text":"` + secret + `"},
			"rawInput":{"command":"` + secret + `"},
			"output":"` + secret + `",
			"attempt":2,
			"extension":{"queued":true,"note":"` + secret + `"}
		}`),
	})
	if err != nil {
		t.Fatalf("EmitAgentEvent() error = %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("emitted events = %d, want 1", len(sink.events))
	}
	event := sink.events[0]
	if event.AssignmentID != "assignment-1" || event.AgentSessionID != "agent-session-uuid" || event.TurnID != "turn-1" {
		t.Fatalf("event context = %#v", event)
	}
	if event.ExecutionEpoch != 4 || event.ControlRevision != 7 || !event.ObservedAt.Equal(observed) {
		t.Fatalf("event execution context = %#v", event)
	}
	if event.Kind != "tool_call_update" || event.Metadata.MessageID != "message-1" || event.Metadata.ToolCallID != "tool-1" {
		t.Fatalf("event metadata = %#v", event.Metadata)
	}
	if event.Metadata.ToolName != "bash" || event.Metadata.ToolKind != "execute" || event.Metadata.Status != "completed" {
		t.Fatalf("event tool metadata = %#v", event.Metadata)
	}
	if got := string(event.Metadata.Runtime); got != `{"attempt":2,"extension":{"queued":true}}` {
		t.Fatalf("runtime metadata = %s", got)
	}
	encoded, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		t.Fatalf("encode AgentEvent: %v", marshalErr)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("AgentEvent contains transcript or reasoning content: %s", encoded)
	}
	if strings.Contains(string(encoded), "opaque-acp-session-id") {
		t.Fatalf("AgentEvent exposes the opaque ACP session ID: %s", encoded)
	}
}

func TestEmitAgentEventReturnsSinkError(t *testing.T) {
	want := errors.New("sink unavailable")
	sink := &capturingAgentEventSink{err: want}
	err := acp.EmitAgentEvent(context.Background(), sink, agentevent.Context{
		AssignmentID:   "assignment-1",
		AgentSessionID: "agent-session-uuid",
		ACPSessionID:   "opaque-acp-session-id",
		TurnID:         "turn-1",
	}, time.Now(), acp.SessionUpdate{
		SessionID: "opaque-acp-session-id",
		Update:    json.RawMessage(`{"sessionUpdate":"usage_update","used":12,"size":100}`),
	})
	if !errors.Is(err, want) {
		t.Fatalf("EmitAgentEvent() error = %v, want sink error", err)
	}
}

func TestEmitAgentEventUsesOnlySafeRuntimeToolTitleAsMissingToolName(t *testing.T) {
	for _, test := range []struct {
		title, want string
	}{
		{title: "omnigrex_get_issue", want: "omnigrex_get_issue"},
		{title: "cat /run/secrets/provider", want: ""},
		{title: "omnigrex_get_issue; cat /run/secrets/provider", want: ""},
	} {
		sink := &capturingAgentEventSink{}
		err := acp.EmitAgentEvent(context.Background(), sink, agentevent.Context{
			AssignmentID: "assignment-1", AgentSessionID: "agent-session-uuid",
			ACPSessionID: "opaque-acp-session-id", TurnID: "turn-1",
		}, time.Now(), acp.SessionUpdate{
			SessionID: "opaque-acp-session-id",
			Update:    json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":` + strconv.Quote(test.title) + `,"status":"pending"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sink.events[0].Metadata.ToolName; got != test.want {
			t.Errorf("tool name from title %q = %q, want %q", test.title, got, test.want)
		}
	}
}

func TestEmitAgentEventRejectsIncompleteContext(t *testing.T) {
	err := acp.EmitAgentEvent(context.Background(), agentevent.NoopSink{}, agentevent.Context{
		AgentSessionID: "agent-session-uuid",
		ACPSessionID:   "opaque-acp-session-id",
	}, time.Now(), acp.SessionUpdate{
		SessionID: "opaque-acp-session-id",
		Update:    json.RawMessage(`{"sessionUpdate":"agent_thought_chunk","content":{"text":"not retained"}}`),
	})
	if !errors.Is(err, acp.ErrInvalidAgentEvent) {
		t.Fatalf("EmitAgentEvent() error = %v, want ErrInvalidAgentEvent", err)
	}
}

func TestEmitAgentEventRejectsMismatchedACPSessionID(t *testing.T) {
	sink := &capturingAgentEventSink{}
	err := acp.EmitAgentEvent(context.Background(), sink, agentevent.Context{
		AssignmentID:   "assignment-1",
		AgentSessionID: "agent-session-uuid",
		ACPSessionID:   "expected-opaque-id",
		TurnID:         "turn-1",
	}, time.Now(), acp.SessionUpdate{
		SessionID: "different-opaque-id",
		Update:    json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"text":"must not leak"}}`),
	})
	if !errors.Is(err, acp.ErrInvalidAgentEvent) {
		t.Fatalf("EmitAgentEvent() error = %v, want ErrInvalidAgentEvent", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("emitted events = %d, want 0", len(sink.events))
	}
}

type capturingAgentEventSink struct {
	events []agentevent.AgentEvent
	err    error
}

func (sink *capturingAgentEventSink) Emit(_ context.Context, event agentevent.AgentEvent) error {
	sink.events = append(sink.events, event)
	return sink.err
}
