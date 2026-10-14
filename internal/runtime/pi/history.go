package pi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

// Replay reads complete append-order history, not the compaction-aware current
// branch. Raw content stays inside the adapter, just as with live Agent Events.
func (c *connection) Replay(ctx context.Context, cursor agent.HistoryCursor, sink agentevent.Sink) (agent.HistoryCursor, error) {
	ctx, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	c.mu.Lock()
	if !c.ready || c.failure != nil {
		c.mu.Unlock()
		return cursor, agent.ErrNotReady
	}
	if c.active != nil || c.cancelling != 0 {
		c.mu.Unlock()
		return cursor, agent.ErrBusy
	}
	id := c.sessionID
	if cursor.RuntimeSessionID != "" && cursor.RuntimeSessionID != id || cursor.EntryID != "" && cursor.RuntimeSessionID == "" {
		c.mu.Unlock()
		return cursor, agent.ErrSession
	}
	c.cancelling++ // Replay and prompt admission are mutually exclusive.
	c.replaying = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.cancelling--; c.replaying = false; c.mu.Unlock() }()
	fields := map[string]string{}
	if cursor.EntryID != "" {
		fields["since"] = cursor.EntryID
	}
	command := "get_entries" // Unowned protocol-only probes retain the native command.
	if c.options.RequirePersistence {
		command = "omnigrex_get_history"
	}
	if sink == nil {
		sink = agentevent.NoopSink{}
	}
	cursor.RuntimeSessionID = id
	seen := map[string]bool{}
	snapshot := ""
	for {
		result, err := c.request(ctx, command, fields)
		if err != nil {
			return cursor, err
		}
		var history struct {
			Entries   []json.RawMessage `json:"entries"`
			More      *bool             `json:"more"`
			Snapshot  string            `json:"snapshot"`
			SessionID string            `json:"sessionId"`
		}
		if json.Unmarshal(result.data, &history) != nil || history.Entries == nil {
			return cursor, agent.ErrProtocol
		}
		if c.options.RequirePersistence {
			if history.More == nil || history.SessionID != id || !validEntryID(history.Snapshot) || snapshot != "" && snapshot != history.Snapshot || *history.More && len(history.Entries) == 0 {
				return cursor, agent.ErrProtocol
			}
			snapshot = history.Snapshot
		}
		for _, raw := range history.Entries {
			event, entryID, err := c.historyEvent(raw)
			if err != nil || seen[entryID] || entryID == cursor.EntryID {
				return cursor, agent.ErrProtocol
			}
			seen[entryID] = true
			// A stalled human/history consumer must not block transport cleanup or
			// exceed the replay deadline, even if it ignores context cancellation.
			done := make(chan error, 1)
			go func() { done <- sink.Emit(ctx, event) }()
			select {
			case err := <-done:
				if err != nil {
					return cursor, agent.ErrBackpressure
				}
			case <-ctx.Done():
				return cursor, ctx.Err()
			}
			cursor.EntryID = entryID
		}
		if !c.options.RequirePersistence || !*history.More {
			return cursor, nil
		}
		fields["since"], fields["snapshot"] = cursor.EntryID, snapshot
	}
}

func (c *connection) historyEvent(raw json.RawMessage) (agentevent.AgentEvent, string, error) {
	var entry struct {
		ID        string    `json:"id"`
		ParentID  *string   `json:"parentId"`
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			Role       string `json:"role"`
			StopReason string `json:"stopReason"`
			IsError    bool   `json:"isError"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &entry) != nil || !validEntryID(entry.ID) || entry.ParentID != nil && !validEntryID(*entry.ParentID) || entry.Timestamp.IsZero() {
		return agentevent.AgentEvent{}, "", agent.ErrProtocol
	}
	kind := "history_" + entry.Type
	status, failure := "", ""
	switch entry.Type {
	case "message":
		switch entry.Message.Role {
		case "user", "system", "assistant", "toolResult", "bashExecution", "custom":
		default:
			return agentevent.AgentEvent{}, "", agent.ErrProtocol
		}
		kind = "history_" + entry.Message.Role + "_message"
		if entry.Message.Role == "assistant" {
			switch entry.Message.StopReason {
			case "stop", "length":
				status = "completed"
			case "toolUse":
				status = "tool_use"
			case "error":
				status, failure = "failed", "provider_failure"
			case "aborted":
				status = "aborted"
			case "pending", "deferred":
				status = "unknown"
			default:
				return agentevent.AgentEvent{}, "", agent.ErrProtocol
			}
		}
		if entry.Message.Role == "toolResult" && entry.Message.IsError {
			status = "failed"
		}
	case "model_change", "thinking_level_change", "usage", "compaction", "branch_summary", "custom", "custom_message", "context_edit", "label", "session_info":
	default:
		return agentevent.AgentEvent{}, "", agent.ErrProtocol
	}
	c.mu.Lock()
	event := c.eventLocked(kind, status, failure)
	c.mu.Unlock()
	event.ObservedAt = entry.Timestamp.UTC()
	event.Metadata.MessageID = entry.ID
	event.Metadata.Runtime, _ = json.Marshal(struct {
		Runtime   string  `json:"runtime"`
		EntryType string  `json:"entry_type"`
		ParentID  *string `json:"parent_id"`
	}{"pi-session/v3", entry.Type, entry.ParentID})
	return event, entry.ID, nil
}

func validEntryID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
