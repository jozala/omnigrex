package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const maxFrameSize = 16 << 20

const maxConcurrentAgentRequests = 32

const (
	ParseError       = -32700
	InvalidRequest   = -32600
	MethodNotFound   = -32601
	InvalidParams    = -32602
	InternalError    = -32603
	RequestCancelled = -32800
)

var ErrConnectionClosed = errors.New("ACP connection closed")

type Handler interface {
	HandleRequest(context.Context, string, json.RawMessage) (any, *RPCError)
	HandleNotification(context.Context, string, json.RawMessage)
}

type RPCError struct {
	Code    int32           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func NewRPCError(code int32, message string) *RPCError {
	return &RPCError{Code: code, Message: message}
}

func (err *RPCError) Error() string {
	return fmt.Sprintf("ACP error %d: %s", err.Code, err.Message)
}

type Connection struct {
	transport io.ReadWriteCloser
	handler   Handler
	ctx       context.Context
	cancel    context.CancelFunc

	nextID        atomic.Int64
	writes        chan writeRequest
	notifications chan notification
	requests      chan struct{}
	done          chan struct{}

	mutex       sync.Mutex
	pending     map[string]chan callResponse
	terminalErr error
	closeOnce   sync.Once
}

type writeRequest struct {
	data    []byte
	done    chan error
	started chan bool
	ctx     context.Context
}

type notification struct {
	method string
	params json.RawMessage
}

type callResponse struct {
	result json.RawMessage
	err    *RPCError
}

type incomingRequestError struct {
	code    int32
	message string
	id      json.RawMessage
}

func (err *incomingRequestError) Error() string {
	return err.message
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

func NewConnection(transport io.ReadWriteCloser, handler Handler) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	connection := &Connection{
		transport:     transport,
		handler:       handler,
		ctx:           ctx,
		cancel:        cancel,
		writes:        make(chan writeRequest, 64),
		notifications: make(chan notification, 256),
		requests:      make(chan struct{}, maxConcurrentAgentRequests),
		done:          make(chan struct{}),
		pending:       make(map[string]chan callResponse),
	}
	go connection.readLoop()
	go connection.writeLoop()
	go connection.notificationLoop()
	return connection
}

func (connection *Connection) Call(ctx context.Context, method string, params, result any) error {
	return connection.call(ctx, method, params, result, nil)
}

func (connection *Connection) call(
	ctx context.Context,
	method string,
	params any,
	result any,
	submitted chan<- error,
) error {
	return connection.callWithContexts(ctx, ctx, method, params, result, submitted)
}

func (connection *Connection) callWithContexts(
	sendCtx context.Context,
	waitCtx context.Context,
	method string,
	params any,
	result any,
	submitted chan<- error,
) error {
	reportSubmission := func(err error) {
		if submitted != nil {
			submitted <- err
		}
	}
	if method == "" {
		err := errors.New("ACP method is empty")
		reportSubmission(err)
		return err
	}

	id := connection.nextID.Add(1)
	key := fmt.Sprintf("n:%d", id)
	response := make(chan callResponse, 1)

	connection.mutex.Lock()
	if connection.terminalErr != nil {
		err := connection.terminalErr
		connection.mutex.Unlock()
		reportSubmission(err)
		return err
	}
	connection.pending[key] = response
	connection.mutex.Unlock()
	defer connection.removePending(key)

	if err := connection.sendJSON(sendCtx, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		reportSubmission(err)
		return err
	}
	reportSubmission(nil)

	select {
	case received := <-response:
		return decodeCallResponse(received, result)
	case <-waitCtx.Done():
		connection.removePending(key)
		cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = connection.Notify(cancelCtx, "$/cancel_request", map[string]any{"requestId": id})
		return waitCtx.Err()
	case <-connection.done:
		return connection.err()
	}
}

func (connection *Connection) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return errors.New("ACP method is empty")
	}
	return connection.sendJSON(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (connection *Connection) Close() error {
	connection.terminate(ErrConnectionClosed)
	return nil
}

func (connection *Connection) readLoop() {
	scanner := bufio.NewScanner(connection.transport)
	scanner.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scanner.Scan() {
		frame := bytes.TrimSpace(scanner.Bytes())
		if err := connection.handleFrame(frame); err != nil {
			var requestErr *incomingRequestError
			if errors.As(err, &requestErr) {
				if sendErr := connection.sendJSON(connection.ctx, map[string]any{
					"jsonrpc": "2.0",
					"id":      requestErr.id,
					"error":   NewRPCError(requestErr.code, requestErr.message),
				}); sendErr != nil {
					connection.terminate(fmt.Errorf("write ACP protocol error: %w", sendErr))
					return
				}
				continue
			}
			connection.terminate(fmt.Errorf("read ACP message: %w", err))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		connection.terminate(fmt.Errorf("read ACP message: %w", err))
		return
	}
	connection.terminate(io.EOF)
}

func (connection *Connection) writeLoop() {
	for {
		select {
		case request := <-connection.writes:
			var err error
			select {
			case <-request.ctx.Done():
				request.started <- false
				err = request.ctx.Err()
			default:
				request.started <- true
				err = writeAll(connection.transport, request.data)
			}
			request.done <- err
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				connection.terminate(fmt.Errorf("write ACP message: %w", err))
				return
			}
		case <-connection.done:
			return
		}
	}
}

func (connection *Connection) notificationLoop() {
	for {
		select {
		case message := <-connection.notifications:
			connection.handler.HandleNotification(connection.ctx, message.method, message.params)
		case <-connection.done:
			return
		}
	}
}

func (connection *Connection) handleFrame(frame []byte) error {
	if !json.Valid(frame) {
		return &incomingRequestError{code: ParseError, message: "parse error", id: json.RawMessage("null")}
	}
	if len(frame) == 0 || frame[0] != '{' {
		return &incomingRequestError{code: InvalidRequest, message: "invalid request", id: json.RawMessage("null")}
	}
	var message wireMessage
	if err := json.Unmarshal(frame, &message); err != nil {
		return &incomingRequestError{code: InvalidRequest, message: "invalid request", id: json.RawMessage("null")}
	}
	if message.JSONRPC != "2.0" {
		if message.Method != "" || (message.ID == nil && message.Result == nil && message.Error == nil) {
			id := message.ID
			if id == nil {
				id = json.RawMessage("null")
			}
			return &incomingRequestError{code: InvalidRequest, message: "invalid request", id: id}
		}
		return fmt.Errorf("unsupported JSON-RPC version %q", message.JSONRPC)
	}

	if message.Method != "" {
		if message.ID == nil {
			if connection.handler != nil {
				params := append(json.RawMessage(nil), message.Params...)
				select {
				case connection.notifications <- notification{method: message.Method, params: params}:
				case <-connection.done:
				}
			}
			return nil
		}
		select {
		case connection.requests <- struct{}{}:
			go func() {
				defer func() { <-connection.requests }()
				connection.handleRequest(message)
			}()
		case <-connection.done:
		}
		return nil
	}

	if message.ID == nil {
		return &incomingRequestError{code: InvalidRequest, message: "invalid request", id: json.RawMessage("null")}
	}
	if (message.Result == nil) == (message.Error == nil) {
		return errors.New("response must contain exactly one of result or error")
	}
	key, err := requestIDKey(message.ID)
	if err != nil {
		return err
	}

	connection.mutex.Lock()
	pending := connection.pending[key]
	if pending != nil {
		delete(connection.pending, key)
	}
	connection.mutex.Unlock()
	if pending != nil {
		pending <- callResponse{result: message.Result, err: message.Error}
	}
	return nil
}

func (connection *Connection) handleRequest(message wireMessage) {
	result, rpcErr := connection.callHandler(message.Method, message.Params)
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      message.ID,
	}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	_ = connection.sendJSON(connection.ctx, response)
}

func (connection *Connection) callHandler(method string, params json.RawMessage) (result any, rpcErr *RPCError) {
	if connection.handler == nil {
		return nil, NewRPCError(MethodNotFound, "method not found")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			rpcErr = NewRPCError(InternalError, "request handler failed")
		}
	}()
	return connection.handler.HandleRequest(connection.ctx, method, params)
}

func (connection *Connection) sendJSON(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode ACP message: %w", err)
	}
	data = append(data, '\n')
	request := writeRequest{
		data:    data,
		done:    make(chan error, 1),
		started: make(chan bool, 1),
		ctx:     ctx,
	}

	select {
	case connection.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-connection.done:
		return connection.err()
	}

	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		select {
		case err := <-request.done:
			return err
		default:
		}
		select {
		case started := <-request.started:
			if started {
				if writer, ok := connection.transport.(interface{ SetWriteDeadline(time.Time) error }); ok {
					_ = writer.SetWriteDeadline(time.Now())
					err := <-request.done
					_ = writer.SetWriteDeadline(time.Time{})
					if err == nil {
						return nil
					}
				}
				connection.terminate(ctx.Err())
			}
		default:
			connection.terminate(ctx.Err())
		}
		return ctx.Err()
	case <-connection.done:
		return connection.err()
	}
}

func (connection *Connection) terminate(err error) {
	connection.closeOnce.Do(func() {
		connection.mutex.Lock()
		connection.terminalErr = err
		clear(connection.pending)
		connection.mutex.Unlock()
		connection.cancel()
		close(connection.done)
		_ = connection.transport.Close()
	})
}

func (connection *Connection) removePending(key string) {
	connection.mutex.Lock()
	delete(connection.pending, key)
	connection.mutex.Unlock()
}

func (connection *Connection) err() error {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.terminalErr == nil {
		return ErrConnectionClosed
	}
	return connection.terminalErr
}

func decodeCallResponse(response callResponse, result any) error {
	if response.err != nil {
		return response.err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.result, result); err != nil {
		return fmt.Errorf("decode ACP result: %w", err)
	}
	return nil
}

func requestIDKey(raw json.RawMessage) (string, error) {
	if bytes.Equal(raw, []byte("null")) {
		return "null", nil
	}
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", fmt.Errorf("invalid string request id: %w", err)
		}
		return "s:" + text, nil
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", fmt.Errorf("invalid request id: %w", err)
	}
	return fmt.Sprintf("n:%d", number), nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
