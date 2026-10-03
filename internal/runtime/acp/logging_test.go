package acp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

// These tests catch missing method/direction correlation and payload leakage at
// the real ACP transport boundary, rather than checking a logging helper alone.
func TestACPLogsRejectedAgentRequest(t *testing.T) {
	logs := captureACPLogs(t)
	client, agent := newPipeClient(t, acp.ClientOptions{})
	client.SetAgentEventContext(agentevent.Context{
		AssignmentID: "assignment-1", AgentSessionID: "session-1", ACPSessionID: "acp-1",
		TurnID: "turn-1", ExecutionEpoch: 11, ControlRevision: 2,
	})
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0", "id": "callback-1", "method": "fs/read_text_file",
		"params": map[string]string{"path": "private-payload-secret"},
	})
	response := readWireMessage(t, bufio.NewReader(agent))
	if response.Result != nil {
		t.Fatal("unsupported request unexpectedly succeeded")
	}
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"level": "WARN", "msg": "ACP request failed", "method": "fs/read_text_file",
		"direction": "agent_to_orchestrator", "request_id": "callback-1", "rpc_code": float64(-32601),
		"assignment_id": "assignment-1", "agent_session_id": "session-1", "acp_session_id": "acp-1",
		"agent_turn_id": "turn-1", "execution_epoch": float64(11), "control_revision": float64(2),
	})
	assertACPDuration(t, record)
	assertNoACPPayload(t, record)
}

func TestACPLogsRemoteRequestFailureWithoutRemotePayload(t *testing.T) {
	logs := captureACPLogs(t)
	client, agent := newPipeClient(t, acp.ClientOptions{})
	client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", TurnID: "original-turn", ExecutionEpoch: 11})
	done := make(chan error, 1)
	go func() { _, err := client.Initialize(context.Background()); done <- err }()
	request := readWireMessage(t, bufio.NewReader(agent))
	client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", TurnID: "later-turn", ExecutionEpoch: 12})
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0", "id": request.ID,
		"error": map[string]any{"code": -32601, "message": "private-payload-secret", "data": map[string]string{"token": "private-payload-secret"}},
	})
	var rpcErr *acp.RPCError
	if err := <-done; !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("Initialize error = %v, want method not found", err)
	}
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"level": "WARN", "msg": "ACP request failed", "method": "initialize",
		"direction": "orchestrator_to_agent", "request_id": float64(1), "rpc_code": float64(-32601),
		"failure_class": "rpc_error", "failure_stage": "response",
		"agent_turn_id": "original-turn", "execution_epoch": float64(11),
	})
	assertACPDuration(t, record)
	assertNoACPPayload(t, record)
}

func TestACPLogsCancelledRequestAsExpected(t *testing.T) {
	logs := captureACPLogs(t)
	connection, agent := newPipeConnection(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- connection.Call(ctx, "session/prompt", map[string]string{"prompt": "private-payload-secret"}, nil)
	}()
	reader := bufio.NewReader(agent)
	_ = readWireMessage(t, reader)
	cancel()
	_ = readWireMessage(t, reader)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want cancellation", err)
	}
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"level": "INFO", "method": "session/prompt", "direction": "orchestrator_to_agent",
		"failure_class": "cancelled", "failure_stage": "response",
	})
	assertNoACPPayload(t, record)
}

func TestACPLogsUnexpectedDisconnectAndPendingMethod(t *testing.T) {
	logs := captureACPLogs(t)
	connection, agent := newPipeConnection(t, nil)
	done := make(chan error, 1)
	go func() { done <- connection.Call(context.Background(), "session/list", nil, nil) }()
	_ = readWireMessage(t, bufio.NewReader(agent))
	_ = agent.Close()
	if err := <-done; err == nil {
		t.Fatal("Call succeeded after disconnect")
	}
	// Connection termination and request completion can log in either order.
	records := []map[string]any{logs.next(t), logs.next(t)}
	for _, record := range records {
		if record["msg"] == "ACP connection closed" {
			assertACPFields(t, record, map[string]any{"level": "WARN", "failure_class": "eof", "failure_stage": "read", "pending_requests": float64(1)})
		} else {
			assertACPFields(t, record, map[string]any{"level": "WARN", "msg": "ACP request failed", "method": "session/list", "failure_class": "eof"})
		}
	}
}

func TestACPLogsProtocolRejectionWithoutFrame(t *testing.T) {
	logs := captureACPLogs(t)
	_, agent := newPipeConnection(t, nil)
	if _, err := agent.Write([]byte("private-payload-secret\n")); err != nil {
		t.Fatal(err)
	}
	_ = readWireMessage(t, bufio.NewReader(agent))
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"level": "WARN", "msg": "ACP protocol failed", "direction": "agent_to_orchestrator",
		"rpc_code": float64(-32700), "failure_class": "protocol_error", "failure_stage": "read",
	})
	assertNoACPPayload(t, record)
}

func TestACPLogsExplicitCloseAsExpected(t *testing.T) {
	logs := captureACPLogs(t)
	connection, _ := newPipeConnection(t, nil)
	_ = connection.Close()
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"level": "INFO", "msg": "ACP connection closed", "failure_class": "closed", "failure_stage": "close",
	})
}

func TestACPUsesConfiguredLoggerBeforeSessionBinding(t *testing.T) {
	logs := &acpLogCapture{records: make(chan map[string]any, 100)}
	client, agent := newPipeClient(t, acp.ClientOptions{
		Logger: slog.New(slog.NewJSONHandler(logs, nil)).With("workflow_id", "workflow-1"),
		DiagnosticContext: agentevent.Context{
			AssignmentID: "assignment-1", AgentSessionID: "session-1", TurnID: "turn-1", ExecutionEpoch: 11,
		},
	})
	// Prepare clears event emission context during initialization/recovery.
	client.SetAgentEventContext(agentevent.Context{})
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "unknown"})
	_ = readWireMessage(t, bufio.NewReader(agent))
	assertACPFields(t, logs.next(t), map[string]any{
		"workflow_id": "workflow-1", "assignment_id": "assignment-1", "agent_session_id": "session-1",
		"agent_turn_id": "turn-1", "execution_epoch": float64(11),
	})
}

func TestACPLogsFailedNotification(t *testing.T) {
	logs := captureACPLogs(t)
	connection, _ := newPipeConnection(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connection.Notify(ctx, "session/cancel", map[string]string{"secret": "private-payload-secret"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Notify error = %v", err)
	}
	record := logs.next(t)
	if record["msg"] == "ACP connection closed" {
		record = logs.next(t)
	}
	assertACPFields(t, record, map[string]any{
		"msg": "ACP notification failed", "level": "INFO", "method": "session/cancel",
		"direction": "orchestrator_to_agent", "failure_class": "cancelled",
	})
	assertNoACPPayload(t, record)
}

func TestACPDoesNotLogStructuredRequestIDs(t *testing.T) {
	for _, id := range []any{map[string]string{"token": "private-payload-secret"}, []string{"private-payload-secret"}} {
		logs := captureACPLogs(t)
		_, agent := newPipeClient(t, acp.ClientOptions{})
		writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": id, "method": "unknown"})
		_ = readWireMessage(t, bufio.NewReader(agent))
		record := logs.next(t)
		if record["request_id"] != nil {
			t.Errorf("structured request ID logged: %v", record)
		}
		assertNoACPPayload(t, record)
	}
}

func TestACPLogsLargeRequestIDExactly(t *testing.T) {
	var logs bytes.Buffer
	_, agent := newPipeClient(t, acp.ClientOptions{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": int64(9007199254740993), "method": "unknown"})
	_ = readWireMessage(t, bufio.NewReader(agent))
	var record struct {
		ID json.RawMessage `json:"request_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if string(record.ID) != "9007199254740993" {
		t.Errorf("request ID = %s, want exact integer", record.ID)
	}
}

func TestACPLogsRecoverableProtocolRequestIdentity(t *testing.T) {
	logs := captureACPLogs(t)
	_, agent := newPipeConnection(t, nil)
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "1.0", "id": "bad-version-1", "method": "fs/read_text_file"})
	_ = readWireMessage(t, bufio.NewReader(agent))
	assertACPFields(t, logs.next(t), map[string]any{
		"msg": "ACP protocol failed", "method": "fs/read_text_file", "request_id": "bad-version-1", "rpc_code": float64(-32600),
	})
}

func TestACPLogsInitializationValidationFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		version   int
		required  acp.RequiredCapabilities
		wantErr   error
		wantClass string
	}{
		{"unsupported_version", 2, acp.RequiredCapabilities{}, acp.ErrProtocolVersion, "protocol_version_mismatch"},
		{"missing_capability", 1, acp.RequiredCapabilities{SessionResume: true}, acp.ErrRequiredCapability, "required_capability_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := &acpLogCapture{records: make(chan map[string]any, 100)}
			client, agent := newPipeClient(t, acp.ClientOptions{
				Logger:               slog.New(slog.NewJSONHandler(logs, nil)).With("workflow_id", "workflow-1"),
				DiagnosticContext:    agentevent.Context{AssignmentID: "assignment-1", AgentSessionID: "session-1", TurnID: "original-turn", ExecutionEpoch: 11},
				RequiredCapabilities: test.required,
			})
			done := make(chan error, 1)
			go func() { _, err := client.Initialize(context.Background()); done <- err }()
			request := readWireMessage(t, bufio.NewReader(agent))
			// Validation must use the same snapshot as the outgoing initialize call.
			client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", TurnID: "later-turn", ExecutionEpoch: 12})
			writeWireMessage(t, agent, map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"protocolVersion": test.version, "agentCapabilities": map[string]any{}, "private": "private-payload-secret"},
			})
			if err := <-done; !errors.Is(err, test.wantErr) {
				t.Fatalf("Initialize error = %v, want %v", err, test.wantErr)
			}
			record := logs.next(t)
			assertACPFields(t, record, map[string]any{
				"level": "WARN", "msg": "ACP request failed", "method": "initialize", "request_id": float64(1),
				"direction": "orchestrator_to_agent", "failure_stage": "validate", "failure_class": test.wantClass,
				"workflow_id": "workflow-1", "assignment_id": "assignment-1", "agent_session_id": "session-1",
				"agent_turn_id": "original-turn", "execution_epoch": float64(11),
			})
			assertACPDuration(t, record)
			assertNoACPPayload(t, record)
		})
	}
}

func TestACPLogsSessionResponseValidationFailures(t *testing.T) {
	oversized := make([]map[string]string, 101)
	for index := range oversized {
		oversized[index] = map[string]string{"sessionId": "opaque-session", "cwd": acp.WorkspacePath}
	}
	for _, test := range []struct {
		name      string
		method    string
		result    any
		wantErr   error
		wantClass string
	}{
		{"unknown_prompt_stop_reason", "session/prompt", map[string]string{"stopReason": "private-payload-secret"}, acp.ErrUnknownStopReason, "unknown_stop_reason"},
		{"empty_created_session", "session/new", map[string]string{"sessionId": "", "private": "private-payload-secret"}, acp.ErrSessionIDEmpty, "empty_session_id"},
		{"empty_discovered_session", "session/list", map[string]any{"sessions": []map[string]string{{"sessionId": "", "cwd": "private-payload-secret"}}}, acp.ErrSessionIDEmpty, "empty_session_id"},
		{"oversized_page", "session/list", map[string]any{"sessions": oversized}, acp.ErrSessionDiscoveryLimit, "session_discovery_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := &acpLogCapture{records: make(chan map[string]any, 100)}
			client, agent := newPipeClient(t, acp.ClientOptions{
				Logger: slog.New(slog.NewJSONHandler(logs, nil)).With("workflow_id", "workflow-1"),
			})
			reader := bufio.NewReader(agent)
			initializeClient(t, client, agent, reader, map[string]any{"sessionCapabilities": map[string]any{"list": map[string]any{}}})
			createSession(t, client, agent, reader, "opaque-session")
			client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", AgentSessionID: "session-1", TurnID: "original-turn", ExecutionEpoch: 11})
			done := make(chan error, 1)
			var promptResponse acp.PromptResponse
			var newSession acp.Session
			var page acp.ListSessionsResponse
			go func() {
				var err error
				switch test.method {
				case "session/prompt":
					promptResponse, err = client.Prompt(context.Background(), "opaque-session", []acp.ContentBlock{acp.TextContent("private-payload-secret")})
				case "session/new":
					newSession, err = client.CreateSession(context.Background(), acp.CreateSessionRequest{CWD: acp.WorkspacePath})
				case "session/list":
					page, err = client.DiscoverSessions(context.Background(), acp.WorkspacePath, "")
				}
				done <- err
			}()
			request := readWireMessage(t, reader)
			if request.Method != test.method {
				t.Fatalf("method = %q, want %q", request.Method, test.method)
			}
			client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", TurnID: "later-turn", ExecutionEpoch: 12})
			writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": test.result})
			if err := <-done; !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.method == "session/prompt" && string(promptResponse.StopReason) != "private-payload-secret" {
				t.Fatalf("unknown stop reason was not preserved: %+v", promptResponse)
			}
			if newSession.ID != "" || len(page.Sessions) != 0 {
				t.Fatal("invalid session response was not cleared")
			}
			record := logs.next(t)
			var requestID float64
			if err := json.Unmarshal(request.ID, &requestID); err != nil {
				t.Fatal(err)
			}
			assertACPFields(t, record, map[string]any{
				"level": "WARN", "msg": "ACP request failed", "method": test.method, "request_id": requestID,
				"direction": "orchestrator_to_agent", "failure_stage": "validate", "failure_class": test.wantClass,
				"workflow_id": "workflow-1", "assignment_id": "assignment-1", "agent_session_id": "session-1",
				"agent_turn_id": "original-turn", "execution_epoch": float64(11),
			})
			assertACPDuration(t, record)
			assertNoACPPayload(t, record)
			// Validation failures must leave the connection usable and not duplicate logs.
			createSession(t, client, agent, reader, "next-session")
			select {
			case extra := <-logs.records:
				t.Fatalf("unexpected additional log: %v", extra)
			default:
			}
		})
	}
}

func TestACPLogsInvalidPromptResponseAfterCancellation(t *testing.T) {
	logs := captureACPLogs(t)
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "opaque-session")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() { response, err := client.Prompt(ctx, "opaque-session", nil); done <- outcome{response, err} }()
	request := readWireMessage(t, reader)
	cancel()
	if notification := readWireMessage(t, reader); notification.Method != "session/cancel" {
		t.Fatalf("unexpected cancellation method: %s", notification.Method)
	}
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"stopReason": "private-payload-secret"}})
	result := <-done
	if !errors.Is(result.err, acp.ErrUnknownStopReason) || string(result.response.StopReason) != "private-payload-secret" {
		t.Fatalf("Prompt result changed: %+v", result)
	}
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{"level": "WARN", "method": "session/prompt", "failure_stage": "validate", "failure_class": "unknown_stop_reason"})
	assertNoACPPayload(t, record)
}

func TestACPLogsCancellationGraceExpiryAsTimeout(t *testing.T) {
	logs := &acpLogCapture{records: make(chan map[string]any, 100)}
	client, agent := newPipeClient(t, acp.ClientOptions{
		Logger:                  slog.New(slog.NewJSONHandler(logs, nil)).With("workflow_id", "workflow-1"),
		CancellationGracePeriod: 100 * time.Millisecond,
	})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "opaque-session")
	client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", AgentSessionID: "session-1", TurnID: "original-turn", ExecutionEpoch: 11})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := client.Prompt(ctx, "opaque-session", []acp.ContentBlock{acp.TextContent("private-payload-secret")})
		done <- outcome{response, err}
	}()
	request := readWireMessage(t, reader)
	client.SetAgentEventContext(agentevent.Context{AssignmentID: "assignment-1", TurnID: "later-turn", ExecutionEpoch: 12})
	cancel()
	if notification := readWireMessage(t, reader); notification.Method != "session/cancel" {
		t.Fatalf("unexpected cancellation method: %s", notification.Method)
	}
	// Read the cancellation but deliberately withhold the terminal prompt response.
	select {
	case result := <-done:
		if !errors.Is(result.err, context.DeadlineExceeded) || result.response.StopReason != "" ||
			!strings.Contains(result.err.Error(), "ACP prompt did not stop after cancellation") {
			t.Fatalf("Prompt result changed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt did not enforce cancellation grace")
	}
	var requestID float64
	if err := json.Unmarshal(request.ID, &requestID); err != nil {
		t.Fatal(err)
	}
	seenRequest, seenClose := false, false
	for range 2 {
		record := logs.next(t)
		assertACPFields(t, record, map[string]any{"level": "WARN", "failure_class": "deadline_exceeded"})
		assertNoACPPayload(t, record)
		switch record["msg"] {
		case "ACP request failed":
			seenRequest = true
			assertACPFields(t, record, map[string]any{
				"method": "session/prompt", "request_id": requestID, "direction": "orchestrator_to_agent",
				"failure_stage": "response", "workflow_id": "workflow-1", "assignment_id": "assignment-1",
				"agent_session_id": "session-1", "agent_turn_id": "original-turn", "execution_epoch": float64(11),
			})
			assertACPDuration(t, record)
		case "ACP connection closed":
			seenClose = true
			assertACPFields(t, record, map[string]any{"failure_stage": "cancellation_grace", "pending_requests": float64(1)})
		default:
			t.Fatalf("unexpected log: %v", record)
		}
	}
	if !seenRequest || !seenClose {
		t.Fatal("missing prompt or connection timeout diagnostic")
	}
	_ = agent.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("Runtime Process transport was not closed: %v", err)
	}
}

func TestACPLogsTypedInvalidEnvelopeIdentityWithoutChangingResponse(t *testing.T) {
	for _, test := range []struct {
		name       string
		frame      map[string]any
		wantMethod string
		wantID     any
	}{
		{"numeric_version", map[string]any{"jsonrpc": 7, "id": "callback-1", "method": "fs/read_text_file"}, "fs/read_text_file", "callback-1"},
		{"invalid_error", map[string]any{"jsonrpc": "2.0", "id": 9, "method": "fs/read_text_file", "error": "private-payload-secret"}, "fs/read_text_file", float64(9)},
		{"structured_id", map[string]any{"jsonrpc": 7, "id": map[string]string{"token": "private-payload-secret"}, "method": "fs/read_text_file"}, "fs/read_text_file", nil},
		{"structured_method", map[string]any{"jsonrpc": "2.0", "id": "callback-1", "method": map[string]string{"token": "private-payload-secret"}}, "", "callback-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureACPLogs(t)
			_, agent := newPipeClient(t, acp.ClientOptions{})
			test.frame["params"] = map[string]string{"secret": "private-payload-secret"}
			writeWireMessage(t, agent, test.frame)
			reader := bufio.NewReader(agent)
			responseLine, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				ID    json.RawMessage `json:"id"`
				Error acp.RPCError    `json:"error"`
			}
			if err := json.Unmarshal(responseLine, &response); err != nil {
				t.Fatal(err)
			}
			if string(response.ID) != "null" || response.Error.Code != -32600 {
				t.Fatalf("protocol rejection changed: %s", responseLine)
			}
			record := logs.next(t)
			assertACPFields(t, record, map[string]any{
				"level": "WARN", "msg": "ACP protocol failed", "method": test.wantMethod,
				"request_id": test.wantID, "rpc_code": float64(-32600),
			})
			assertNoACPPayload(t, record)
			// Malformed-envelope diagnostics must not terminate a recoverable connection.
			writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": "next-1", "method": "unknown"})
			if next := readWireMessage(t, reader); string(next.ID) != `"next-1"` {
				t.Fatalf("connection did not handle next request: %s", next.ID)
			}
		})
	}
}

func TestACPLogsBlockedWriteCancellationAsExpected(t *testing.T) {
	logs := captureACPLogs(t)
	transport := &deadlineLoggingTransport{blockingTransport: newBlockingTransport(), expired: make(chan struct{})}
	connection := acp.NewConnection(transport, nil)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- connection.Call(ctx, "session/prompt", nil, nil) }()
	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled write did not stop")
	}
	for range 2 {
		record := logs.next(t)
		assertACPFields(t, record, map[string]any{"level": "INFO", "failure_class": "cancelled"})
	}
}

// Force the deadline-interruption path used by Docker's attachment transport.
type deadlineLoggingTransport struct {
	*blockingTransport
	expired chan struct{}
}

func (transport *deadlineLoggingTransport) Write(_ []byte) (int, error) {
	transport.startOnce.Do(func() { close(transport.writeStarted) })
	select {
	case <-transport.expired:
	case <-transport.closed:
	}
	return 0, errors.New("private-payload-secret")
}

func (transport *deadlineLoggingTransport) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		close(transport.expired)
	}
	return nil
}

func TestACPLogsResultDecodeFailureWithoutPayload(t *testing.T) {
	logs := captureACPLogs(t)
	connection, agent := newPipeConnection(t, nil)
	done := make(chan error, 1)
	go func() {
		var result struct {
			Count int `json:"count"`
		}
		done <- connection.Call(context.Background(), "session/list", nil, &result)
	}()
	request := readWireMessage(t, bufio.NewReader(agent))
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"count": "private-payload-secret"},
	})
	if err := <-done; err == nil {
		t.Fatal("invalid result unexpectedly decoded")
	}
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{"method": "session/list", "failure_stage": "decode", "level": "WARN"})
	assertNoACPPayload(t, record)
}

func TestACPDoesNotLogSuccessfulCallsAsFailures(t *testing.T) {
	logs := captureACPLogs(t)
	connection, agent := newPipeConnection(t, nil)
	done := make(chan error, 1)
	go func() { done <- connection.Call(context.Background(), "session/list", nil, nil) }()
	request := readWireMessage(t, bufio.NewReader(agent))
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"content": "private-payload-secret"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	assertACPFields(t, logs.next(t), map[string]any{"msg": "ACP connection closed", "level": "INFO"})
}

func TestACPLogsHandlerPanicWithoutPanicValue(t *testing.T) {
	logs := captureACPLogs(t)
	_, agent := newPipeConnection(t, testHandler{request: func(context.Context, string, json.RawMessage) (any, *acp.RPCError) {
		panic("private-payload-secret")
	}})
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": "panic-1", "method": "session/request_permission"})
	_ = readWireMessage(t, bufio.NewReader(agent))
	record := logs.next(t)
	assertACPFields(t, record, map[string]any{
		"method": "session/request_permission", "rpc_code": float64(-32603), "failure_stage": "handler", "level": "WARN",
	})
	assertNoACPPayload(t, record)
}

func TestACPLogsDeadlineWithPendingMethod(t *testing.T) {
	logs := captureACPLogs(t)
	connection, agent := newPipeConnection(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- connection.Call(ctx, "session/list", nil, nil) }()
	reader := bufio.NewReader(agent)
	_ = readWireMessage(t, reader)
	_ = readWireMessage(t, reader) // Deadline sends $/cancel_request.
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v, want deadline exceeded", err)
	}
	assertACPFields(t, logs.next(t), map[string]any{
		"method": "session/list", "failure_class": "deadline_exceeded", "failure_stage": "response", "level": "WARN",
	})
}

func TestACPLogsSubmissionFailureWithoutErrorString(t *testing.T) {
	logs := captureACPLogs(t)
	connection, _ := newPipeConnection(t, nil)
	if err := connection.Call(context.Background(), "session/prompt", make(chan string), nil); err == nil {
		t.Fatal("unencodable request succeeded")
	}
	assertACPFields(t, logs.next(t), map[string]any{
		"method": "session/prompt", "failure_class": "local_error", "failure_stage": "submit", "level": "WARN",
	})
}

type acpLogCapture struct{ records chan map[string]any }

func (capture *acpLogCapture) Write(data []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	capture.records <- record
	return len(data), nil
}

func captureACPLogs(t *testing.T) *acpLogCapture {
	t.Helper()
	capture := &acpLogCapture{records: make(chan map[string]any, 100)}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(capture, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

func (capture *acpLogCapture) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case record := <-capture.records:
		return record
	case <-time.After(time.Second):
		t.Fatal("missing ACP operational log")
		return nil
	}
}

func assertACPFields(t *testing.T, record, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if record[key] != value {
			t.Errorf("log[%s] = %v, want %v; record = %v", key, record[key], value, record)
		}
	}
}

func assertACPDuration(t *testing.T, record map[string]any) {
	t.Helper()
	if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("invalid or missing duration_ms: %v", record)
	}
}

func assertNoACPPayload(t *testing.T, record map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "private-payload-secret") {
		t.Errorf("ACP log leaked payload: %s", encoded)
	}
	for _, key := range []string{"params", "result", "data", "error", "rpc_message", "frame"} {
		if _, found := record[key]; found {
			t.Errorf("ACP log includes unsafe field %q", key)
		}
	}
}
