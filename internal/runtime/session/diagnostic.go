package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

// CheckDiagnostic exercises candidate Session coordination in a disposable
// protocol probe. Its in-memory ledger is not production persistence evidence;
// PostgreSQL fencing is qualified separately. It owns no protected resources,
// Workflow, or MCP mutation authority, and does not launch Turn executions.
func CheckDiagnostic(ctx context.Context, connect RuntimeConnect) error {
	s := store.AgentSession{ID: uuid.NewString(), AgentParticipantID: uuid.NewString(), Status: store.AgentSessionCreating, ControlOwner: store.SessionControlAutomation, ControlRevision: 1, NextExecutionEpoch: 2, RuntimeImageDigest: "diagnostic", RuntimeProfileContentSHA256: "diagnostic"}
	lease := store.AgentTurnLease{AgentTurn: store.AgentTurn{ID: uuid.NewString(), AgentSessionID: s.ID, AgentParticipantID: s.AgentParticipantID, ExecutionEpoch: 1}}
	lease.ControlRevision = 1
	lease.JobLease.AgentTurnID, lease.JobLease.AgentSessionID, lease.JobLease.AgentParticipantID, lease.JobLease.ExecutionEpoch = lease.ID, s.ID, s.AgentParticipantID, 1
	ledger := &diagnosticBindingStore{session: s}
	coordinator := NewRuntimeCoordinator(ledger)
	state := execution.StateRef{ID: s.AgentParticipantID, ParticipantID: s.AgentParticipantID, SessionID: s.ID, Backend: execution.BackendBinding{Family: "diagnostic", Version: 1, Namespace: uuid.NewString()}, Format: "pi-session/v3", RuntimeProfileSHA256: s.RuntimeProfileContentSHA256, Image: s.RuntimeImageDigest}
	result, err := coordinator.Prepare(ctx, RuntimePrepareRequest{Lease: lease, Session: s, State: state, Connect: connect})
	if err != nil {
		return err
	}
	outcome, err := coordinator.Prompt(ctx, RuntimePromptRequest{Lease: lease, Session: result.Session, Connection: result.Connection, Text: "configuration-audit"})
	if err != nil || outcome.Status != agent.Completed {
		_ = result.Connection.Close()
		return errors.Join(err, agent.ErrRuntime)
	}
	ledger.mu.Lock()
	ledger.session.ControlOwner, ledger.session.ControlRevision = store.SessionControlHuman, 2
	human := ledger.session
	ledger.mu.Unlock()
	outcome, err = coordinator.HumanPrompt(ctx, RuntimeHumanPromptRequest{Session: human, Connect: func(context.Context, agent.SessionIdentity) (agent.Connection, error) { return result.Connection, nil }, Text: "configuration-audit", Sink: agentevent.NoopSink{}})
	if err != nil || outcome.Status != agent.Completed {
		return errors.Join(err, agent.ErrRuntime)
	}
	return nil
}

type diagnosticBindingStore struct {
	mu      sync.Mutex
	session store.AgentSession
	token   string
}

func (d *diagnosticBindingStore) ValidateTurnFence(ctx context.Context, lease store.AgentTurnLease) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if d.session.ID != lease.AgentSessionID || d.session.ControlOwner != store.SessionControlAutomation || d.session.ControlRevision != lease.ControlRevision {
		return store.ErrAgentTurnFenceLost
	}
	return nil
}
func (d *diagnosticBindingStore) ReserveAgentSessionRuntime(ctx context.Context, lease store.AgentTurnLease) (store.AgentSession, error) {
	if err := d.ValidateTurnFence(ctx, lease); err != nil {
		return store.AgentSession{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.session.RuntimeSessionID = d.session.ID
	return d.session, nil
}
func (d *diagnosticBindingStore) BindAgentSessionRuntime(ctx context.Context, lease store.AgentTurnLease, ready agent.Ready) (store.AgentSession, error) {
	if err := d.ValidateTurnFence(ctx, lease); err != nil {
		return store.AgentSession{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ready.RuntimeSessionID != d.session.ID || !json.Valid(ready.Capabilities) {
		return store.AgentSession{}, agent.ErrSession
	}
	d.session.RuntimeCapabilities, d.session.Status = ready.Capabilities, store.AgentSessionActive
	return d.session, nil
}
func (*diagnosticBindingStore) MarkAgentTurnStateInitialized(context.Context, store.AgentTurnLease, execution.StateRef) error {
	return nil
}
func (d *diagnosticBindingStore) ValidateRuntimeSessionPromptFence(ctx context.Context, lease store.AgentTurnLease, id string) error {
	if err := d.ValidateTurnFence(ctx, lease); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if id != d.session.RuntimeSessionID {
		return agent.ErrSession
	}
	return nil
}
func (d *diagnosticBindingStore) AcquireHumanPromptLease(ctx context.Context, id string, revision int64, runtimeID string, duration time.Duration) (store.HumanPromptLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return store.HumanPromptLease{}, ctx.Err()
	}
	if d.session.ID != id || d.session.ControlRevision != revision || d.session.ControlOwner != store.SessionControlHuman || d.session.RuntimeSessionID != runtimeID || d.token != "" {
		return store.HumanPromptLease{}, store.ErrHumanPromptLeaseActive
	}
	d.token = uuid.NewString()
	return store.HumanPromptLease{AgentSessionID: id, ControlRevision: revision, RuntimeSessionID: runtimeID, Token: d.token, LeaseExpiresAt: time.Now().Add(duration)}, nil
}
func (d *diagnosticBindingStore) HeartbeatHumanPromptLease(ctx context.Context, lease store.HumanPromptLease, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if d.token != lease.Token || d.session.ControlRevision != lease.ControlRevision {
		return store.ErrHumanPromptLeaseLost
	}
	return nil
}
func (d *diagnosticBindingStore) ReleaseHumanPromptLease(_ context.Context, lease store.HumanPromptLease) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != lease.Token {
		return store.ErrHumanPromptLeaseLost
	}
	d.token = ""
	return nil
}
