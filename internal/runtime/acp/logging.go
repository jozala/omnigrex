package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"
)

// Only protocol metadata is logged. In particular, RPCError.Message/Data and
// arbitrary transport or decoder error strings may contain sensitive payloads.
func logRequestFailure(logger *slog.Logger, method string, id any, direction, stage string, started time.Time, err error) {
	level, class := diagnosticFailure(err)
	attributes := []any{
		"method", method, "request_id", id, "direction", direction,
		"failure_stage", stage, "failure_class", class,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		attributes = append(attributes, "rpc_code", rpcErr.Code)
	}
	logger.Log(context.Background(), level, "ACP request failed", attributes...)
}

func diagnosticFailure(err error) (slog.Level, string) {
	var rpcErr *RPCError
	switch {
	case errors.Is(err, context.Canceled):
		return slog.LevelInfo, "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return slog.LevelWarn, "deadline_exceeded"
	case errors.Is(err, ErrConnectionClosed):
		return slog.LevelInfo, "closed"
	case errors.Is(err, io.EOF):
		return slog.LevelWarn, "eof"
	case errors.Is(err, ErrProtocolVersion):
		return slog.LevelWarn, "protocol_version_mismatch"
	case errors.Is(err, ErrRequiredCapability):
		return slog.LevelWarn, "required_capability_missing"
	case errors.Is(err, ErrUnknownStopReason):
		return slog.LevelWarn, "unknown_stop_reason"
	case errors.Is(err, ErrSessionIDEmpty):
		return slog.LevelWarn, "empty_session_id"
	case errors.Is(err, ErrSessionDiscoveryLimit):
		return slog.LevelWarn, "session_discovery_limit"
	case errors.As(err, &rpcErr):
		if rpcErr.Code == RequestCancelled {
			return slog.LevelInfo, "cancelled"
		}
		return slog.LevelWarn, "rpc_error"
	default:
		return slog.LevelWarn, "local_error"
	}
}

func diagnosticRequestID(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			return id
		}
		return nil
	}
	var id int64
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	return nil
}

// Decode only allowlisted raw fields so typed-invalid envelope fields cannot
// hide recoverable identity. This is diagnostic-only; rejection responses keep
// their original IDs, including null when the envelope cannot be decoded.
func diagnosticFrameMetadata(frame []byte) (string, any) {
	var fields struct {
		Method json.RawMessage `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(frame, &fields) != nil {
		return "", nil
	}
	var method string
	if json.Unmarshal(fields.Method, &method) != nil {
		method = ""
	}
	return method, diagnosticRequestID(fields.ID)
}

// A deadline used to interrupt an intentional cancellation can surface as a
// transport timeout. Preserve the returned error, but attribute its diagnostic
// to the cancellation that caused it, including for other pending requests.
func (connection *Connection) diagnosticError(err error) error {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.terminalErr != nil &&
		(errors.Is(err, connection.terminalErr) || errors.Is(connection.terminalErr, err)) {
		return connection.terminalLogErr
	}
	return err
}

func (client *Client) diagnosticLogger() *slog.Logger {
	client.mutex.Lock()
	eventContext := client.agentEventContext
	client.mutex.Unlock()
	if eventContext.AssignmentID == "" {
		eventContext = client.options.DiagnosticContext
	}
	return client.options.Logger.With(
		"assignment_id", eventContext.AssignmentID, "agent_session_id", eventContext.AgentSessionID,
		"acp_session_id", eventContext.ACPSessionID, "agent_turn_id", eventContext.TurnID,
		"execution_epoch", eventContext.ExecutionEpoch, "control_revision", eventContext.ControlRevision,
	)
}
