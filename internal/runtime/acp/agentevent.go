package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

var ErrInvalidAgentEvent = errors.New("invalid ACP Agent Event")

func EmitAgentEvent(
	ctx context.Context,
	sink agentevent.Sink,
	eventContext agentevent.Context,
	observedAt time.Time,
	update SessionUpdate,
) error {
	if sink == nil || eventContext.AssignmentID == "" || eventContext.AgentSessionID == "" || eventContext.ACPSessionID == "" || eventContext.TurnID == "" ||
		observedAt.IsZero() || update.SessionID == "" || update.SessionID != eventContext.ACPSessionID || len(update.Update) == 0 {
		return ErrInvalidAgentEvent
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(update.Update, &fields); err != nil {
		return ErrInvalidAgentEvent
	}
	kind, err := requiredStringField(fields, "sessionUpdate")
	if err != nil {
		return err
	}
	metadata := agentevent.OperationalMetadata{}
	if metadata.MessageID, err = optionalStringField(fields, "messageId"); err != nil {
		return err
	}
	if metadata.ToolCallID, err = optionalStringField(fields, "toolCallId"); err != nil {
		return err
	}
	if metadata.ToolName, err = optionalStringField(fields, "toolName"); err != nil {
		return err
	}
	if metadata.ToolKind, err = optionalStringField(fields, "kind"); err != nil {
		return err
	}
	if metadata.Status, err = optionalStringField(fields, "status"); err != nil {
		return err
	}
	if metadata.ModeID, err = optionalStringField(fields, "currentModeId"); err != nil {
		return err
	}
	if metadata.UsedTokens, err = optionalUintField(fields, "used"); err != nil {
		return err
	}
	if metadata.ContextSize, err = optionalUintField(fields, "size"); err != nil {
		return err
	}
	if raw, ok := fields["cost"]; ok {
		var cost struct {
			Amount   *float64 `json:"amount"`
			Currency string   `json:"currency"`
		}
		if err := json.Unmarshal(raw, &cost); err != nil || cost.Amount == nil || *cost.Amount < 0 || cost.Currency == "" {
			return ErrInvalidAgentEvent
		}
		metadata.CostAmount = cost.Amount
		metadata.CostCurrency = cost.Currency
	}
	if raw, ok := fields["availableCommands"]; ok {
		var commands []json.RawMessage
		if err := json.Unmarshal(raw, &commands); err != nil {
			return ErrInvalidAgentEvent
		}
		count := uint64(len(commands))
		metadata.AvailableCommandCount = &count
	}

	for _, key := range []string{
		"sessionUpdate", "messageId", "toolCallId", "toolName", "kind", "status", "currentModeId",
		"used", "size", "cost", "availableCommands",
	} {
		delete(fields, key)
	}
	if runtimeFields := sanitizeRuntimeFields(fields); len(runtimeFields) > 0 {
		encoded, err := json.Marshal(runtimeFields)
		if err != nil {
			return ErrInvalidAgentEvent
		}
		metadata.Runtime = encoded
	}

	event := agentevent.AgentEvent{
		AssignmentID:    eventContext.AssignmentID,
		AgentSessionID:  eventContext.AgentSessionID,
		TurnID:          eventContext.TurnID,
		ExecutionEpoch:  eventContext.ExecutionEpoch,
		ControlRevision: eventContext.ControlRevision,
		Kind:            agentevent.Kind(kind),
		ObservedAt:      observedAt,
		Metadata:        metadata,
	}
	if err := sink.Emit(ctx, event); err != nil {
		return fmt.Errorf("emit ACP Agent Event: %w", err)
	}
	return nil
}

func requiredStringField(fields map[string]json.RawMessage, name string) (string, error) {
	value, err := optionalStringField(fields, name)
	if err != nil || value == "" {
		return "", ErrInvalidAgentEvent
	}
	return value, nil
}

func optionalStringField(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", ErrInvalidAgentEvent
	}
	return value, nil
}

func optionalUintField(fields map[string]json.RawMessage, name string) (*uint64, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, nil
	}
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrInvalidAgentEvent
	}
	return &value, nil
}

func sanitizeRuntimeFields(fields map[string]json.RawMessage) map[string]any {
	result := make(map[string]any)
	for key, raw := range fields {
		if sensitiveMetadataKey(key) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			continue
		}
		if safe, ok := sanitizeRuntimeValue(key, value); ok {
			result[key] = safe
		}
	}
	return result
}

func sanitizeRuntimeValue(key string, value any) (any, bool) {
	switch value := value.(type) {
	case nil:
		return nil, true
	case bool, json.Number:
		return value, true
	case string:
		if safeOperationalStringKey(key) {
			return value, true
		}
		return nil, false
	case map[string]any:
		result := make(map[string]any)
		for childKey, child := range value {
			if sensitiveMetadataKey(childKey) {
				continue
			}
			if safe, ok := sanitizeRuntimeValue(childKey, child); ok {
				result[childKey] = safe
			}
		}
		return result, len(result) > 0
	case []any:
		result := make([]any, 0, len(value))
		for _, child := range value {
			safe, ok := sanitizeRuntimeValue(key, child)
			if !ok {
				return nil, false
			}
			result = append(result, safe)
		}
		return result, true
	default:
		return nil, false
	}
}

func sensitiveMetadataKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
	for _, fragment := range []string{"content", "text", "prompt", "reason", "thought", "input", "output", "message", "title", "description", "command"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func safeOperationalStringKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
	return strings.HasSuffix(normalized, "id") || normalized == "status" || normalized == "state" ||
		normalized == "kind" || normalized == "type" || normalized == "phase" || normalized == "currency"
}
