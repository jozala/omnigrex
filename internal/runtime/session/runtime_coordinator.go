package session

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

// RuntimeBindingStore owns identity reservations, control and admission. Agent
// implementations never receive these database operations or lease authority.
type RuntimeBindingStore interface {
	ValidateTurnFence(context.Context, store.AgentTurnLease) error
	ReserveAgentSessionRuntime(context.Context, store.AgentTurnLease) (store.AgentSession, error)
	BindAgentSessionRuntime(context.Context, store.AgentTurnLease, agent.Ready) (store.AgentSession, error)
	MarkAgentTurnStateInitialized(context.Context, store.AgentTurnLease, execution.StateRef) error
	ValidateRuntimeSessionPromptFence(context.Context, store.AgentTurnLease, string) error
	AcquireHumanPromptLease(context.Context, string, int64, string, time.Duration) (store.HumanPromptLease, error)
	HeartbeatHumanPromptLease(context.Context, store.HumanPromptLease, time.Duration) error
	ReleaseHumanPromptLease(context.Context, store.HumanPromptLease) error
}

// RuntimeConnect installs exact ephemeral inputs only after durable admission.
// Environment stop/removal and exclusive state attachment belong to its owner.
type RuntimeConnect func(context.Context, agent.SessionIdentity) (agent.Connection, error)

type RuntimePrepareRequest struct {
	Lease   store.AgentTurnLease
	Session store.AgentSession
	State   execution.StateRef
	Connect RuntimeConnect
}

type RuntimeResult struct {
	Session     store.AgentSession
	Connection  agent.Connection
	Disposition Disposition
}

type RuntimePromptRequest struct {
	Lease      store.AgentTurnLease
	Session    store.AgentSession
	Connection agent.Connection
	Text       string
	Sink       agentevent.Sink
}

type RuntimeHumanPromptRequest struct {
	Session           store.AgentSession
	Connect           RuntimeConnect
	Text              string
	Sink              agentevent.Sink
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
}

type RuntimeCoordinator struct{ store RuntimeBindingStore }

func NewRuntimeCoordinator(bindingStore RuntimeBindingStore) *RuntimeCoordinator {
	return &RuntimeCoordinator{store: bindingStore}
}

// Prepare reserves identity before calling the adapter, and revalidates the
// fence before adopting its result. It never authorizes or submits a prompt.
func (c *RuntimeCoordinator) Prepare(ctx context.Context, request RuntimePrepareRequest) (RuntimeResult, error) {
	if c == nil || c.store == nil || request.Connect == nil {
		return RuntimeResult{}, errors.New("prepare Agent Session: dependency is nil")
	}
	s, lease, ref := request.Session, request.Lease, request.State
	if !runtimeTurnIdentity(s, lease) || s.NextExecutionEpoch-1 != lease.ExecutionEpoch || s.Status != store.AgentSessionCreating && s.Status != store.AgentSessionActive {
		return RuntimeResult{}, ErrTurnIdentityMismatch
	}
	if ref.Validate() != nil || ref.ParticipantID != s.AgentAssignmentID || ref.SessionID != s.ID || ref.RuntimeProfileSHA256 != s.RuntimeProfileContentSHA256 || ref.Image != s.RuntimeImageDigest {
		return RuntimeResult{}, ErrStatePathMismatch
	}
	reserved, err := c.store.ReserveAgentSessionRuntime(ctx, lease)
	if err != nil {
		return RuntimeResult{}, err
	}
	if reserved.ID != s.ID || reserved.RuntimeSessionID == "" {
		return RuntimeResult{}, agent.ErrSession
	}
	if err := c.store.ValidateTurnFence(ctx, lease); err != nil {
		return RuntimeResult{}, err
	}
	connection, err := request.Connect(ctx, agent.SessionIdentity{AgentSessionID: reserved.ID, RuntimeSessionID: reserved.RuntimeSessionID, MustExist: len(reserved.RuntimeCapabilities) != 0})
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return RuntimeResult{}, err
	}
	if connection == nil {
		return RuntimeResult{}, agent.ErrNotReady
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = connection.Close()
		}
	}()
	connection.SetAgentEventContext(runtimeEventContext(lease, reserved))
	ready, err := connection.Prepare(ctx)
	if err != nil {
		return RuntimeResult{}, err
	}
	if ready.RuntimeSessionID != reserved.RuntimeSessionID {
		return RuntimeResult{}, agent.ErrSession
	}
	if err := c.store.ValidateTurnFence(ctx, lease); err != nil {
		return RuntimeResult{}, err
	}
	bound, err := c.store.BindAgentSessionRuntime(ctx, lease, ready)
	if err != nil {
		return RuntimeResult{}, err
	}
	if err := c.store.MarkAgentTurnStateInitialized(ctx, lease, ref); err != nil {
		return RuntimeResult{}, err
	}
	adopted = true
	return RuntimeResult{Session: bound, Connection: connection, Disposition: Disposition(ready.Disposition)}, nil
}

func (c *RuntimeCoordinator) Prompt(ctx context.Context, request RuntimePromptRequest) (agent.Outcome, error) {
	if c == nil || c.store == nil || request.Connection == nil {
		return agent.Outcome{Status: agent.Rejected}, agent.ErrNotReady
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return agent.Outcome{Status: agent.Rejected}, errors.New("Agent Turn prompt requires a deadline")
	}
	if !runtimeTurnIdentity(request.Session, request.Lease) || request.Session.Status != store.AgentSessionActive || request.Session.RuntimeSessionID == "" {
		return agent.Outcome{Status: agent.Rejected}, ErrTurnIdentityMismatch
	}
	if request.Connection.PreparedSessionID() != request.Session.RuntimeSessionID {
		return agent.Outcome{Status: agent.Rejected}, agent.ErrSession
	}
	if err := c.store.ValidateRuntimeSessionPromptFence(ctx, request.Lease, request.Session.RuntimeSessionID); err != nil {
		return agent.Outcome{Status: agent.Rejected}, err
	}
	request.Connection.SetAgentEventContext(runtimeEventContext(request.Lease, request.Session))
	return request.Connection.Prompt(ctx, request.Text, request.Sink)
}

// HumanPrompt holds one durable admission from exact reopening through complete
// replay and prompt settlement. Uncertain execution preserves its token, denying
// another writer until the execution owner establishes a stopped boundary.
func (c *RuntimeCoordinator) HumanPrompt(ctx context.Context, request RuntimeHumanPromptRequest) (agent.Outcome, error) {
	s := request.Session
	if c == nil || c.store == nil || request.Connect == nil || request.Sink == nil || s.ID == "" || s.RuntimeSessionID == "" || s.ControlRevision <= 0 || s.ControlOwner != store.SessionControlHuman || s.Status != store.AgentSessionActive && s.Status != store.AgentSessionRetained {
		return agent.Outcome{Status: agent.Rejected}, ErrHumanPromptIdentityMismatch
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return agent.Outcome{Status: agent.Rejected}, errors.New("human prompt requires a deadline")
	}
	duration, interval, err := humanPromptTiming(HumanPromptRequest{LeaseDuration: request.LeaseDuration, HeartbeatInterval: request.HeartbeatInterval})
	if err != nil {
		return agent.Outcome{Status: agent.Rejected}, err
	}
	lease, err := c.store.AcquireHumanPromptLease(ctx, s.ID, s.ControlRevision, s.RuntimeSessionID, duration)
	if err != nil {
		return agent.Outcome{Status: agent.Rejected}, err
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	done := make(chan error, 1)
	go func() {
		err := heartbeatHumanPromptLease(heartbeatCtx, c.store, lease, duration, interval)
		if err != nil {
			cancel(err)
		}
		done <- err
	}()
	outcome := agent.Outcome{Status: agent.Rejected}
	connection, operationErr := request.Connect(workCtx, agent.SessionIdentity{AgentSessionID: s.ID, RuntimeSessionID: s.RuntimeSessionID, MustExist: true})
	if connection != nil {
		defer func() { _ = connection.Close() }()
	}
	if operationErr == nil && connection == nil {
		operationErr = agent.ErrNotReady
	}
	if operationErr == nil {
		connection.SetAgentEventContext(agentevent.Context{AssignmentID: s.AgentAssignmentID, AgentSessionID: s.ID, ControlRevision: uint64(s.ControlRevision)})
		var ready agent.Ready
		ready, operationErr = connection.Prepare(workCtx)
		if operationErr == nil && ready.RuntimeSessionID != s.RuntimeSessionID {
			operationErr = agent.ErrSession
		}
		if operationErr == nil {
			actual, err := canonicalJSON(ready.Capabilities)
			expected, expectedErr := canonicalJSON(s.RuntimeCapabilities)
			if err != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
				operationErr = ErrCapabilityMismatch
			}
		}
		if operationErr == nil {
			_, operationErr = connection.Replay(workCtx, agent.HistoryCursor{}, request.Sink)
		}
		if operationErr == nil {
			operationErr = c.store.HeartbeatHumanPromptLease(workCtx, lease, duration)
		}
		if operationErr == nil {
			outcome, operationErr = connection.Prompt(workCtx, request.Text, request.Sink)
		}
		_ = connection.Close()
	}
	stopHeartbeat()
	heartbeatErr := <-done
	cancel(nil)
	operationErr = errors.Join(operationErr, heartbeatErr)
	if outcome.Status == agent.Unknown {
		return outcome, errors.Join(operationErr, ErrHumanPromptOutcomeUncertain)
	}
	releaseCtx, stopRelease := context.WithTimeout(context.WithoutCancel(ctx), humanPromptReleaseTimeout)
	defer stopRelease()
	if err := c.store.ReleaseHumanPromptLease(releaseCtx, lease); err != nil {
		operationErr = errors.Join(operationErr, err, ErrHumanPromptOutcomeUncertain)
	}
	return outcome, operationErr
}

func runtimeTurnIdentity(s store.AgentSession, lease store.AgentTurnLease) bool {
	return s.ID != "" && s.ID == lease.AgentSessionID && s.AgentAssignmentID != "" && s.AgentAssignmentID == lease.AgentAssignmentID && s.ControlOwner == store.SessionControlAutomation && s.ControlRevision > 0 && s.ControlRevision == lease.ControlRevision && lease.ExecutionEpoch > 0 && lease.JobLease.AgentSessionID == s.ID && lease.JobLease.AgentAssignmentID == s.AgentAssignmentID && lease.JobLease.AgentTurnID == lease.ID && lease.JobLease.ExecutionEpoch == lease.ExecutionEpoch
}

func runtimeEventContext(lease store.AgentTurnLease, s store.AgentSession) agentevent.Context {
	return agentevent.Context{AssignmentID: s.AgentAssignmentID, AgentSessionID: s.ID, TurnID: lease.ID, ExecutionEpoch: uint64(lease.ExecutionEpoch), ControlRevision: uint64(lease.ControlRevision)}
}
