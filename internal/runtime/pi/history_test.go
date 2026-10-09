package pi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

type historySink struct {
	events []agentevent.AgentEvent
}

type historySinkFunc func(context.Context, agentevent.AgentEvent) error

func (s historySinkFunc) Emit(ctx context.Context, event agentevent.AgentEvent) error {
	return s(ctx, event)
}

func replayFixture(t *testing.T, entries []any) (agent.Connection, context.Context) {
	t.Helper()
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{RequestTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			data := any(map[string]any{"sessionId": "session", "isStreaming": false})
			if command.Type == "get_entries" {
				data = map[string]any{"entries": entries}
			}
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": data})
		}
	}()
	if _, err := c.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}

func TestReplayCursorAdvancesOnlyThroughAcknowledgedEvents(t *testing.T) {
	entries := []any{
		map[string]any{"id": "one", "parentId": nil, "type": "message", "timestamp": "2026-10-09T12:00:00Z", "message": map[string]any{"role": "user", "content": "secret-provider-key"}},
		map[string]any{"id": "two", "parentId": "one", "type": "message", "timestamp": "2026-10-09T12:00:01Z", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "secret-provider-key"}},
	}
	c, ctx := replayFixture(t, entries)
	emitted := 0
	cursor, err := c.Replay(ctx, agent.HistoryCursor{}, historySinkFunc(func(_ context.Context, event agentevent.AgentEvent) error {
		emitted++
		if strings.Contains(fmt.Sprint(event), "secret-provider-key") {
			t.Error("transcript entered operational replay")
		}
		if emitted == 2 {
			return errors.New("sink secret-provider-key")
		}
		return nil
	}))
	if !errors.Is(err, agent.ErrBackpressure) || cursor.EntryID != "one" || strings.Contains(fmt.Sprint(err), "secret-provider-key") {
		t.Fatalf("acknowledged cursor: %+v %v", cursor, err)
	}
}

func TestReplayExcludesPromptAndHasBoundedSlowConsumer(t *testing.T) {
	c, ctx := replayFixture(t, []any{map[string]any{"id": "one", "parentId": nil, "type": "custom", "timestamp": "2026-10-09T12:00:00Z"}})
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		_, err := c.Replay(ctx, agent.HistoryCursor{}, historySinkFunc(func(context.Context, agentevent.AgentEvent) error { close(entered); <-release; return nil }))
		done <- err
	}()
	<-entered
	if outcome, err := c.Prompt(ctx, "must-not-overlap", nil); !errors.Is(err, agent.ErrBusy) || outcome.Status != agent.Rejected {
		t.Fatalf("prompt during replay: %+v %v", outcome, err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded replay: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("replay ignored its budget")
	}
}

func TestReplayRejectsMalformedAndDuplicateHistory(t *testing.T) {
	entry := map[string]any{"id": "one", "parentId": nil, "type": "custom", "timestamp": "2026-10-09T12:00:00Z"}
	for _, entries := range [][]any{{entry, entry}, {map[string]any{"id": "one", "type": "unknown", "timestamp": "2026-10-09T12:00:00Z"}}, {map[string]any{"id": "one", "type": "message", "timestamp": "bad"}}} {
		c, ctx := replayFixture(t, entries)
		if _, err := c.Replay(ctx, agent.HistoryCursor{}, nil); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("corrupt history: %v", err)
		}
	}
}

func (s *historySink) Emit(_ context.Context, event agentevent.AgentEvent) error {
	s.events = append(s.events, event)
	return nil
}

func TestHistoryReplayUsesAppendOrderAndCursorWithoutPrompting(t *testing.T) {
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{ExpectedSessionID: "session", EventContext: agentevent.Context{AgentSessionID: "durable", AssignmentID: "participant"}})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	commands := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type, Since string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			commands <- command.Type
			data := any(map[string]any{"sessionId": "session", "isStreaming": false})
			if command.Type == "get_entries" {
				entries := []any{
					map[string]any{"id": "a", "parentId": nil, "timestamp": "2026-10-09T12:00:00Z", "type": "message", "message": map[string]any{"role": "user", "content": "secret transcript"}},
					map[string]any{"id": "b", "parentId": "a", "timestamp": "2026-10-09T12:00:01Z", "type": "message", "message": map[string]any{"role": "assistant", "stopReason": "stop"}},
					map[string]any{"id": "c", "parentId": "a", "timestamp": "2026-10-09T12:00:02Z", "type": "compaction", "summary": "secret summary"},
				}
				if command.Since == "c" {
					entries = []any{}
				}
				data = map[string]any{"entries": entries, "leafId": "c"}
			}
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": data})
		}
	}()
	if _, err := c.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	sink := &historySink{}
	cursor, err := c.Replay(ctx, agent.HistoryCursor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.RuntimeSessionID != "session" || cursor.EntryID != "c" {
		t.Fatalf("cursor: %+v", cursor)
	}
	var kinds []agentevent.Kind
	for _, event := range sink.events {
		kinds = append(kinds, event.Kind)
		if event.AgentSessionID != "durable" || event.AssignmentID != "participant" {
			t.Fatal("replay lost durable correlation")
		}
	}
	if !reflect.DeepEqual(kinds, []agentevent.Kind{"history_user_message", "history_assistant_message", "history_compaction"}) {
		t.Fatalf("kinds: %v", kinds)
	}
	before := len(sink.events)
	if got, err := c.Replay(ctx, cursor, sink); err != nil || got != cursor || len(sink.events) != before {
		t.Fatalf("incremental replay: %+v %v", got, err)
	}
	for range 3 {
		if got := <-commands; got != "get_state" && got != "get_entries" {
			t.Fatalf("replay submitted model activity: %s", got)
		}
	}
}
