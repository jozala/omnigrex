package pi_test

import (
	"net"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

func TestToolEndingsNeedNoPairingOrTimingFallback(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		// Out-of-order, unmatched and duplicate endings are independent observations.
		for _, event := range []map[string]any{
			{"type": "tool_execution_start", "toolName": "read", "toolCallId": "first"},
			{"type": "tool_execution_start", "toolName": "bash", "toolCallId": "second"},
			{"type": "tool_execution_end", "toolName": "bash", "toolCallId": "second", "isError": false, "durationMs": 25},
			{"type": "tool_execution_end", "toolName": "read", "toolCallId": "first", "isError": true},
			{"type": "tool_execution_end", "toolName": "omnigrex_get_issue", "toolCallId": "unmatched", "isError": false, "durationMs": -1},
			{"type": "tool_execution_end", "toolName": "bash", "toolCallId": "second", "isError": false, "durationMs": 25},
			{"type": "tool_execution_end", "toolName": "SENTINEL_SECRET", "isError": false},
			{"type": "tool_execution_end", "toolName": "read", "isError": "SENTINEL_SECRET", "durationMs": map[string]any{}},
		} {
			writeRecord(runtime, event)
		}
		settle(runtime, "stop")
	})
	var tools []*agentevent.ToolObservation
	got, err := c.Prompt(ctx, "tools", eventSinkFunc(func(event agentevent.AgentEvent) {
		if event.Metadata.ToolObservation != nil {
			tools = append(tools, event.Metadata.ToolObservation)
		}
	}))
	if err != nil || got.Status != agent.Completed {
		t.Fatal(got, err)
	}
	if len(tools) != 4 || tools[0].Name != "bash" || *tools[0].Duration != 25*time.Millisecond || !tools[1].Error || tools[1].Duration != nil || tools[2].Layer != "mcp_client" || tools[2].Duration != nil {
		t.Fatalf("tools: %+v", tools)
	}
}

func TestMalformedAndFloodedOptionalFilesystemDoesNotFailPrompt(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		for range 500 {
			writeRecord(runtime, map[string]any{"type": "omnigrex_temporary_filesystems", "data": []any{
				map[string]any{"resource": "tmp", "status": "complete", "used": 42},
				map[string]any{"resource": "run-pi-agent", "status": "unavailable"},
			}})
		}
		for _, data := range []any{"SENTINEL_SECRET", nil, map[string]any{"filename": "SENTINEL_SECRET"}} {
			writeRecord(runtime, map[string]any{"type": "omnigrex_temporary_filesystems", "data": data})
		}
		settle(runtime, "stop")
	})
	outcome, err := c.Prompt(ctx, "optional", agentevent.NoopSink{})
	if err != nil || outcome.Status != agent.Completed {
		t.Fatal(outcome, err)
	}
	// Delivery may coalesce or lose every intermediate sample at settlement.
}
