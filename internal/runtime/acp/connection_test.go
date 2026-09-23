package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
)

func TestConnectionCorrelatesConcurrentCalls(t *testing.T) {
	connection, agent := newPipeConnection(t, nil)
	type result struct {
		Value string `json:"value"`
	}

	results := make(map[string]string)
	errorsByMethod := make(map[string]error)
	var mutex sync.Mutex
	var wait sync.WaitGroup
	for _, method := range []string{"first", "second"} {
		method := method
		wait.Add(1)
		go func() {
			defer wait.Done()
			var response result
			err := connection.Call(context.Background(), method, map[string]string{"method": method}, &response)
			mutex.Lock()
			defer mutex.Unlock()
			results[method] = response.Value
			errorsByMethod[method] = err
		}()
	}

	reader := bufio.NewReader(agent)
	requests := make(map[string]wireMessage)
	for range 2 {
		request := readWireMessage(t, reader)
		requests[request.Method] = request
	}

	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      requests["second"].ID,
		"result":  map[string]string{"value": "second-result"},
	})
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      requests["first"].ID,
		"result":  map[string]string{"value": "first-result"},
	})
	wait.Wait()

	for _, method := range []string{"first", "second"} {
		if errorsByMethod[method] != nil {
			t.Errorf("Call(%q) error = %v", method, errorsByMethod[method])
		}
		if results[method] != method+"-result" {
			t.Errorf("Call(%q) result = %q, want %q", method, results[method], method+"-result")
		}
	}
}

func TestConnectionHandlesAgentMessagesWhileCallIsPending(t *testing.T) {
	notificationReceived := make(chan json.RawMessage, 1)
	handler := testHandler{
		notification: func(_ context.Context, method string, params json.RawMessage) {
			if method == "session/update" {
				notificationReceived <- params
			}
		},
		request: func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RPCError) {
			if method != "session/request_permission" {
				return nil, acp.NewRPCError(acp.MethodNotFound, "unsupported method")
			}
			return map[string]any{
				"outcome": map[string]string{
					"outcome":  "selected",
					"optionId": "allow-once",
				},
			}, nil
		},
	}
	connection, agent := newPipeConnection(t, handler)

	callDone := make(chan error, 1)
	go func() {
		var result struct {
			StopReason string `json:"stopReason"`
		}
		callDone <- connection.Call(context.Background(), "session/prompt", map[string]string{"sessionId": "session-1"}, &result)
	}()

	reader := bufio.NewReader(agent)
	prompt := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params":  map[string]string{"sessionId": "session-1"},
	})
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      "permission-1",
		"method":  "session/request_permission",
		"params":  map[string]string{"sessionId": "session-1"},
	})

	permissionResponse := readWireMessage(t, reader)
	if string(permissionResponse.ID) != `"permission-1"` {
		t.Fatalf("permission response id = %s, want %q", permissionResponse.ID, "permission-1")
	}
	if permissionResponse.Result == nil {
		t.Fatal("permission response result is missing")
	}

	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      prompt.ID,
		"result":  map[string]string{"stopReason": "end_turn"},
	})

	select {
	case <-notificationReceived:
	case <-time.After(time.Second):
		t.Fatal("session/update notification was not handled")
	}
	if err := <-callDone; err != nil {
		t.Fatalf("Call() error = %v", err)
	}
}

func TestConnectionCancelsRequestWhenContextEnds(t *testing.T) {
	connection, agent := newPipeConnection(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		callDone <- connection.Call(ctx, "session/list", map[string]any{}, nil)
	}()

	reader := bufio.NewReader(agent)
	request := readWireMessage(t, reader)
	cancel()
	cancellation := readWireMessage(t, reader)

	if cancellation.Method != "$/cancel_request" {
		t.Fatalf("cancellation method = %q, want %q", cancellation.Method, "$/cancel_request")
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(cancellation.Params, &params); err != nil {
		t.Fatalf("decode cancellation params: %v", err)
	}
	if string(params.RequestID) != string(request.ID) {
		t.Errorf("cancelled request id = %s, want %s", params.RequestID, request.ID)
	}
	if err := <-callDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", err)
	}
}

func TestConnectionFailsPendingCallWhenTransportCloses(t *testing.T) {
	connection, agent := newPipeConnection(t, nil)
	callDone := make(chan error, 1)
	go func() {
		callDone <- connection.Call(context.Background(), "session/list", map[string]any{}, nil)
	}()

	reader := bufio.NewReader(agent)
	_ = readWireMessage(t, reader)
	if err := agent.Close(); err != nil {
		t.Fatalf("close fake agent: %v", err)
	}

	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("Call() error = nil after transport closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Call() remained blocked after transport closed")
	}
}

func TestConnectionCloseInterruptsBlockedWrite(t *testing.T) {
	transport := newBlockingTransport()
	connection := acp.NewConnection(transport, nil)
	callDone := make(chan error, 1)
	go func() {
		callDone <- connection.Call(context.Background(), "session/list", map[string]any{}, nil)
	}()

	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Call() did not begin writing")
	}
	closed := make(chan struct{})
	go func() {
		_ = connection.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() blocked behind the active write")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("Call() error = nil after connection closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Call() remained blocked after connection closed")
	}
}

func TestConnectionCancellationInterruptsBlockedWrite(t *testing.T) {
	transport := newBlockingTransport()
	connection := acp.NewConnection(transport, nil)
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		callDone <- connection.Call(ctx, "session/list", map[string]any{}, nil)
	}()

	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Call() did not begin writing")
	}
	cancel()
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call() ignored cancellation during blocked write")
	}
}

func TestConnectionReportsInvalidMessagesAndContinues(t *testing.T) {
	connection, agent := newPipeConnection(t, nil)
	reader := bufio.NewReader(agent)
	for _, invalid := range []string{"{\n", "{}\n", "null\n", "\n"} {
		if _, err := agent.Write([]byte(invalid)); err != nil {
			t.Fatalf("write invalid message: %v", err)
		}
		response := readWireMessage(t, reader)
		if string(response.ID) != "null" {
			t.Errorf("protocol error id = %s, want null", response.ID)
		}
	}

	callDone := make(chan error, 1)
	go func() {
		callDone <- connection.Call(context.Background(), "session/list", map[string]any{}, nil)
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{},
	})
	if err := <-callDone; err != nil {
		t.Fatalf("Call() after invalid messages = %v", err)
	}
}

type testHandler struct {
	request      func(context.Context, string, json.RawMessage) (any, *acp.RPCError)
	notification func(context.Context, string, json.RawMessage)
}

func (handler testHandler) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, *acp.RPCError) {
	if handler.request == nil {
		return nil, acp.NewRPCError(acp.MethodNotFound, "unsupported method")
	}
	return handler.request(ctx, method, params)
}

func (handler testHandler) HandleNotification(ctx context.Context, method string, params json.RawMessage) {
	if handler.notification != nil {
		handler.notification(ctx, method, params)
	}
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
}

type blockingTransport struct {
	writeStarted chan struct{}
	closed       chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (transport *blockingTransport) Read(_ []byte) (int, error) {
	<-transport.closed
	return 0, io.EOF
}

func (transport *blockingTransport) Write(_ []byte) (int, error) {
	transport.startOnce.Do(func() {
		close(transport.writeStarted)
	})
	<-transport.closed
	return 0, net.ErrClosed
}

func (transport *blockingTransport) Close() error {
	transport.closeOnce.Do(func() {
		close(transport.closed)
	})
	return nil
}

func newPipeConnection(t *testing.T, handler acp.Handler) (*acp.Connection, net.Conn) {
	t.Helper()

	client, agent := net.Pipe()
	connection := acp.NewConnection(client, handler)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = agent.Close()
	})
	return connection, agent
}

func readWireMessage(t *testing.T, reader *bufio.Reader) wireMessage {
	t.Helper()

	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read wire message: %v", err)
	}
	var message wireMessage
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatalf("decode wire message %q: %v", line, err)
	}
	return message
}

func writeWireMessage(t *testing.T, writer net.Conn, message any) {
	t.Helper()

	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("encode wire message: %v", err)
	}
	data = append(data, '\n')
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("write wire message: %v", err)
	}
}
