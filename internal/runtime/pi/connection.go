// Package pi owns Pi's correlated JSONL protocol, never Workflow policy.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

const maxRecordBytes = 1 << 20
const queueCapacity = 128
const maxRequests = 32

type Options struct {
	EventContext        agentevent.Context
	ExpectedSessionID   string
	RequestTimeout      time.Duration
	PromptTimeout       time.Duration
	CancellationTimeout time.Duration
	CloseTimeout        time.Duration
	Selection           *agent.ModelSelection
	RequireMCP          bool
	RequirePersistence  bool
}

type connection struct {
	transport      io.ReadWriteCloser
	options        Options
	mu             sync.Mutex
	nextID         uint64
	pending        map[string]*pendingRequest
	active         *turn
	cancelling     int
	ready          bool
	sessionID      string
	persistedReady *agent.Ready
	replaying      bool
	failure        error
	done           chan struct{}
	writes         chan writeJob
	closeOnce      sync.Once
	closed         chan struct{}
	closeErr       error
}

type pendingRequest struct {
	command string
	result  chan response
}
type response struct {
	acknowledged bool
	success      bool
	data         json.RawMessage
}
type writeJob struct {
	content []byte
	done    chan error
}
type turn struct {
	events          chan agentevent.AgentEvent
	settled         chan struct{}
	settledOK       bool
	lastReason      string
	outcome         agent.Outcome
	retryBackoff    bool
	cancelRequested bool
	retryCancelled  bool
}

// Connect starts reading immediately, including events emitted before a waiter.
// No raw protocol objects, messages, provider errors, or stderr escape the adapter.
func Connect(transport io.ReadWriteCloser, options Options) agent.Connection {
	if options.Selection != nil {
		selection := *options.Selection
		options.Selection = &selection
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.PromptTimeout <= 0 {
		options.PromptTimeout = 5 * time.Minute
	}
	if options.CancellationTimeout <= 0 {
		options.CancellationTimeout = 2 * time.Second
	}
	if options.CloseTimeout <= 0 {
		options.CloseTimeout = 2 * time.Second
	}
	c := &connection{transport: transport, options: options, pending: make(map[string]*pendingRequest), done: make(chan struct{}), writes: make(chan writeJob, maxRequests), closed: make(chan struct{})}
	go c.read()
	go c.write()
	return c
}

func (c *connection) Prepare(ctx context.Context) (agent.Ready, error) {
	c.mu.Lock()
	if c.active != nil || c.replaying || c.cancelling != 0 && (c.options.Selection != nil || c.options.RequireMCP) {
		c.mu.Unlock()
		return agent.Ready{}, agent.ErrBusy
	}
	c.ready = false
	c.cancelling++ // Hold the prompt barrier through checked preparation.
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.cancelling--; c.mu.Unlock() }()
	result, err := c.request(ctx, "get_state", nil)
	if err != nil {
		return agent.Ready{}, err
	}
	var state struct {
		SessionID  string `json:"sessionId"`
		Streaming  *bool  `json:"isStreaming"`
		Compacting bool   `json:"isCompacting"`
		Pending    int    `json:"pendingMessageCount"`
	}
	if json.Unmarshal(result.data, &state) != nil || state.SessionID == "" || len(state.SessionID) > 128 || state.Streaming == nil {
		c.fail(agent.ErrProtocol)
		return agent.Ready{}, agent.ErrProtocol
	}
	if c.options.ExpectedSessionID != "" && state.SessionID != c.options.ExpectedSessionID {
		c.fail(agent.ErrSession)
		return agent.Ready{}, agent.ErrSession
	}
	if *state.Streaming || state.Compacting || state.Pending != 0 {
		return agent.Ready{}, agent.ErrBusy
	}
	if err := c.configure(ctx); err != nil {
		return agent.Ready{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return agent.Ready{}, c.failure
	}
	if c.options.RequirePersistence && (c.persistedReady == nil || c.persistedReady.RuntimeSessionID != state.SessionID) {
		return agent.Ready{}, agent.ErrSession
	}
	c.ready = true
	c.sessionID = state.SessionID
	if c.persistedReady != nil {
		return *c.persistedReady, nil
	}
	return agent.Ready{RuntimeSessionID: state.SessionID}, nil
}

func (c *connection) SetAgentEventContext(owner agentevent.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.options.EventContext = owner
}

func (c *connection) PreparedSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.failure != nil {
		return ""
	}
	return c.sessionID
}

func (c *connection) Prompt(ctx context.Context, text string, sink agentevent.Sink) (outcome agent.Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.options.PromptTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return agent.Outcome{Status: agent.Rejected}, err
	}
	c.mu.Lock()
	if c.failure != nil {
		err = c.failure
	} else if !c.ready {
		err = agent.ErrNotReady
	} else if c.active != nil || c.cancelling != 0 {
		err = agent.ErrBusy
	}
	if err != nil {
		c.mu.Unlock()
		return agent.Outcome{Status: agent.Rejected}, err
	}
	t := &turn{events: make(chan agentevent.AgentEvent, queueCapacity), settled: make(chan struct{})}
	c.active = t
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.active = nil; c.mu.Unlock() }()
	if sink == nil {
		sink = agentevent.NoopSink{}
	}
	// A sink is serial and receives only bounded operational metadata. It cannot
	// block the RPC reader, command correlation, cancellation, or detachment.
	emitJobs := make(chan agentevent.AgentEvent)
	emitResults := make(chan error, 1)
	emitCtx, stopEmitter := context.WithCancel(ctx)
	defer stopEmitter()
	go func() {
		for {
			select {
			case event := <-emitJobs:
				emitResults <- sink.Emit(emitCtx, event)
			case <-emitCtx.Done():
				return
			}
		}
	}()
	emit := func(event agentevent.AgentEvent) error {
		done := c.done
		for {
			select {
			case emitJobs <- event:
				goto sent
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				select {
				case <-t.settled:
					done = nil
				default:
					return c.failureValue()
				}
			}
		}
	sent:
		for {
			select {
			case err := <-emitResults:
				if err != nil {
					return agent.ErrBackpressure
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				select {
				case <-t.settled:
					done = nil
				default:
					return c.failureValue()
				}
			}
		}
	}

	result, err := c.request(ctx, "prompt", map[string]string{"message": text})
	if err != nil {
		if result.acknowledged && errors.Is(err, agent.ErrRuntime) {
			return agent.Outcome{Status: agent.Rejected, FailureClass: "runtime_rejection"}, err
		}
		return c.interrupted(ctx, false, err)
	}
	var admission struct {
		Disposition string `json:"disposition"`
	}
	if json.Unmarshal(result.data, &admission) != nil || admission.Disposition != "started" {
		c.fail(agent.ErrProtocol)
		return agent.Outcome{Status: agent.Unknown, FailureClass: "protocol_failure"}, agent.ErrProtocol
	}
	if err := emit(c.event("prompt_admitted", "running", "")); err != nil {
		return c.interrupted(ctx, true, err)
	}
	for {
		// Drain queued observations before considering EOF or settlement.
		var event agentevent.AgentEvent
		select {
		case event = <-t.events:
		default:
			select {
			case event = <-t.events:
			case <-c.done:
				// Settlement enqueues the terminal event before closing its signal.
				// Continue through that event so neither EOF nor a queue-empty race
				// can return an outcome while discarding terminal diagnostics.
				select {
				case <-t.settled:
					continue
				default:
				}
				return agent.Outcome{Admitted: true, Status: agent.Unknown, FailureClass: failureClass(c.failureValue())}, c.failureValue()
			case <-ctx.Done():
				return c.interrupted(ctx, true, ctx.Err())
			}
		}
		if err := emit(event); err != nil {
			return c.interrupted(ctx, true, err)
		}
		if event.Kind == "prompt_settled" {
			c.mu.Lock()
			outcome = t.outcome
			c.mu.Unlock()
			return outcome, nil
		}
	}
}

func (c *connection) interrupted(ctx context.Context, admitted bool, cause error) (agent.Outcome, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.options.CancellationTimeout)
	defer cancel()
	_ = c.Cancel(cleanup)
	// The timed-out caller cannot reuse this transport to overlap unknown work.
	c.fail(cause)
	return agent.Outcome{Admitted: admitted, Status: agent.Unknown, FailureClass: failureClass(cause)}, cause
}

func (c *connection) Cancel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.options.CancellationTimeout)
	defer cancel()
	c.mu.Lock()
	active := c.active
	if active == nil {
		c.mu.Unlock()
		return nil
	}
	c.cancelling++
	active.cancelRequested = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.cancelling--; c.mu.Unlock() }()
	_, err := c.request(ctx, "abort", nil)
	if err != nil {
		c.fail(err)
		return err
	}
	select {
	case <-active.settled:
		return nil
	case <-ctx.Done():
		select {
		case <-active.settled:
			return nil
		default:
		}
		return ctx.Err()
	case <-c.done:
		select {
		case <-active.settled:
			return nil
		default:
		}
		return c.failureValue()
	}
}

func (c *connection) Close() error {
	c.fail(agent.ErrTransport)
	select {
	case <-c.closed:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closeErr
	case <-time.After(c.options.CloseTimeout):
		return agent.ErrCleanup
	}
}

func (c *connection) request(ctx context.Context, command string, fields map[string]string) (result response, failure error) {
	ctx, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return response{}, err
	}
	if len(c.pending) >= maxRequests {
		c.mu.Unlock()
		return response{}, agent.ErrBackpressure
	}
	c.nextID++
	id := fmt.Sprintf("omnigrex-%d", c.nextID)
	pending := &pendingRequest{command: command, result: make(chan response, 1)}
	c.pending[id] = pending
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	// Correlated responses are authoritative even if the transport reports EOF
	// or its write acknowledgement arrives later than the peer's response.
	defer func() {
		if failure != nil {
			select {
			case result = <-pending.result:
				failure = nil
				if !result.success {
					failure = agent.ErrRuntime
				}
			default:
			}
		}
	}()
	record := map[string]string{"id": id, "type": command}
	for key, value := range fields {
		record[key] = value
	}
	content, _ := json.Marshal(record)
	if len(content) > maxRecordBytes {
		return response{}, agent.ErrProtocol
	}
	job := writeJob{content: append(content, '\n'), done: make(chan error, 1)}
	select {
	case c.writes <- job:
	case <-ctx.Done():
		return response{}, ctx.Err()
	case <-c.done:
		return response{}, c.failureValue()
	}
	written := job.done
	for {
		select {
		case result := <-pending.result:
			if !result.success {
				return result, agent.ErrRuntime
			}
			return result, nil
		case err := <-written:
			if err != nil {
				return response{}, agent.ErrTransport
			}
			written = nil
		case <-ctx.Done():
			if written != nil {
				c.fail(agent.ErrTransport)
			}
			return response{}, ctx.Err()
		case <-c.done:
			return response{}, c.failureValue()
		}
	}
}

func (c *connection) write() {
	for {
		select {
		case job := <-c.writes:
			n, err := c.transport.Write(job.content)
			if err != nil || n != len(job.content) {
				c.fail(agent.ErrTransport)
				job.done <- agent.ErrTransport
				return
			}
			job.done <- nil
		case <-c.done:
			return
		}
	}
}

func (c *connection) read() {
	scanner := bufio.NewScanner(c.transport)
	scanner.Buffer(make([]byte, 4096), maxRecordBytes)
	for scanner.Scan() {
		if err := c.record(scanner.Bytes()); err != nil {
			c.fail(err)
			return
		}
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		c.fail(agent.ErrProtocol)
	} else {
		c.fail(agent.ErrTransport)
	}
}

func (c *connection) record(content []byte) error {
	var record struct {
		Type         string          `json:"type"`
		ID           string          `json:"id"`
		Command      string          `json:"command"`
		Success      *bool           `json:"success"`
		Data         json.RawMessage `json:"data"`
		Message      json.RawMessage `json:"message"`
		FinalError   string          `json:"finalError"`
		SessionID    string          `json:"sessionId"`
		Disposition  string          `json:"disposition"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(content, &record) != nil || record.Type == "" {
		return agent.ErrProtocol
	}
	if record.Type == "omnigrex_configuration_error" {
		return ErrConfiguration
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if record.Type == "omnigrex_session_ready" {
		if c.persistedReady != nil || record.SessionID != c.options.ExpectedSessionID || record.Disposition != "created" && record.Disposition != "recovered" && record.Disposition != "continued" {
			return agent.ErrSession
		}
		c.persistedReady = &agent.Ready{RuntimeSessionID: record.SessionID, Capabilities: record.Capabilities, Disposition: record.Disposition}
		return nil
	}
	if record.Type == "response" {
		if record.ID == "" || record.Success == nil {
			return agent.ErrProtocol
		}
		if pending := c.pending[record.ID]; pending != nil {
			if record.Command != pending.command {
				return agent.ErrProtocol
			}
			select {
			case pending.result <- response{acknowledged: true, success: *record.Success, data: record.Data}:
			default:
				return agent.ErrProtocol
			}
		}
		return nil // Late responses are discarded, never accumulated.
	}
	if record.Type == "extension_error" {
		return agent.ErrRuntime
	}
	t := c.active
	if t == nil {
		return nil
	}
	if t.settledOK {
		return agent.ErrProtocol
	}
	status := "running"
	switch record.Type {
	case "message_start":
		var message struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(record.Message, &message) != nil || message.Role == "" {
			return agent.ErrProtocol
		}
		if message.Role == "assistant" {
			t.lastReason = ""
			t.retryBackoff, t.retryCancelled = false, false
		}
	case "message_end":
		var message struct {
			Role       string `json:"role"`
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(record.Message, &message) != nil || message.Role == "" {
			return agent.ErrProtocol
		}
		if message.Role == "assistant" {
			switch message.StopReason {
			case "stop", "length", "error", "aborted", "toolUse":
			default:
				return agent.ErrProtocol
			}
			t.lastReason = message.StopReason
		}
	case "auto_retry_start":
		t.retryBackoff, t.retryCancelled = true, false
	case "auto_retry_end":
		if record.Success == nil {
			return agent.ErrProtocol
		}
		// Pi 1.0.4 aborts its backoff without appending an aborted assistant
		// message. Recognize its exact cancellation marker only for a requested
		// abort in that backoff; arbitrary provider error text is not evidence.
		t.retryCancelled = t.retryBackoff && t.lastReason == "error" && t.cancelRequested && !*record.Success && record.FinalError == "Retry cancelled"
		t.retryBackoff = false
	case "agent_settled":
		outcome := agent.Outcome{Admitted: true}
		if t.retryCancelled {
			outcome.Status = agent.Aborted
		} else {
			switch t.lastReason {
			case "stop", "length":
				outcome.Status = agent.Completed
			case "error":
				outcome.Status, outcome.FailureClass = agent.Failed, "provider_failure"
			case "aborted":
				outcome.Status = agent.Aborted
			default:
				return agent.ErrProtocol
			}
		}
		t.outcome, t.settledOK = outcome, true
		status = string(outcome.Status)
		select {
		case t.events <- c.runtimeEvent("prompt_settled", status, outcome.FailureClass, record.Type, t.lastReason, t.retryCancelled):
		default:
			return agent.ErrBackpressure
		}
		close(t.settled)
		return nil
	case "agent_start", "agent_end", "message_update", "tool_execution_start", "tool_execution_update", "tool_execution_end", "auto_compaction_start", "auto_compaction_end":
	default:
		return nil
	}
	select {
	case t.events <- c.runtimeEvent("activity", status, "", record.Type, "", t.retryCancelled):
	default:
		return agent.ErrBackpressure
	}
	return nil
}

// Runtime diagnostics are reconstructed from closed-vocabulary observations,
// never copied from source JSON, provider messages, tool arguments, or stderr.
func (c *connection) runtimeEvent(kind, status, failure, source, reason string, retryCancelled bool) agentevent.AgentEvent {
	event := c.eventLocked(kind, status, failure)
	event.Metadata.Runtime, _ = json.Marshal(struct {
		Runtime        string `json:"runtime"`
		Event          string `json:"event"`
		Reason         string `json:"assistant_stop_reason,omitempty"`
		RetryCancelled bool   `json:"retry_cancelled,omitempty"`
	}{"pi-rpc/1.0.4", source, reason, retryCancelled})
	return event
}

func (c *connection) event(kind, status, failure string) agentevent.AgentEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.eventLocked(kind, status, failure)
}

func (c *connection) eventLocked(kind, status, failure string) agentevent.AgentEvent {
	owner := c.options.EventContext
	return agentevent.AgentEvent{AssignmentID: owner.AssignmentID, AgentSessionID: owner.AgentSessionID, TurnID: owner.TurnID, ExecutionEpoch: owner.ExecutionEpoch, ControlRevision: owner.ControlRevision, Kind: agentevent.Kind(kind), ObservedAt: time.Now().UTC(), Metadata: agentevent.OperationalMetadata{Status: status, FailureClass: failure}}
}

func (c *connection) failureValue() error { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }

func (c *connection) fail(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
		close(c.done)
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() {
		go func() {
			err := c.transport.Close()
			c.mu.Lock()
			if err != nil {
				c.closeErr = agent.ErrCleanup
			}
			c.mu.Unlock()
			close(c.closed)
		}()
	})
}

func failureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, agent.ErrProtocol):
		return "protocol_failure"
	case errors.Is(err, agent.ErrBackpressure):
		return "backpressure"
	case errors.Is(err, agent.ErrRuntime):
		return "runtime_failure"
	default:
		return "transport_lost"
	}
}
