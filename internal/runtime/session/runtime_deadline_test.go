package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

type timedDiagnosticConnection struct {
	agent.Connection
	deadline  time.Time
	closed    bool
	completed error
}

func (c *timedDiagnosticConnection) PreparationDeadline() time.Time { return c.deadline }
func (c *timedDiagnosticConnection) Prepare(context.Context) (agent.Ready, error) {
	return agent.Ready{RuntimeSessionID: c.Connection.PreparedSessionID()}, nil
}
func (*timedDiagnosticConnection) SetAgentEventContext(agentevent.Context) {}
func (c *timedDiagnosticConnection) Close() error                          { c.closed = true; return nil }
func (c *timedDiagnosticConnection) CompletePreparation(err error) error {
	c.completed = err
	return err
}

type blockingDiagnosticBinding struct{ *diagnosticBindingStore }

func (b blockingDiagnosticBinding) BindAgentSessionRuntime(ctx context.Context, _ store.AgentTurnLease, _ agent.Ready) (store.AgentSession, error) {
	<-ctx.Done()
	return store.AgentSession{}, ctx.Err()
}

func TestRuntimeCoordinatorRetainsAdapterDeadlineThroughDurableBinding(t *testing.T) {
	id, participant, turnID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := store.AgentSession{ID: id, AgentAssignmentID: participant, Status: store.AgentSessionCreating, ControlOwner: store.SessionControlAutomation, ControlRevision: 1, NextExecutionEpoch: 2, RuntimeImageDigest: "diagnostic", RuntimeProfileContentSHA256: "diagnostic"}
	lease := store.AgentTurnLease{AgentTurn: store.AgentTurn{ID: turnID, AgentSessionID: id, AgentAssignmentID: participant, ExecutionEpoch: 1}}
	lease.ControlRevision = 1
	lease.JobLease.AgentTurnID, lease.JobLease.AgentSessionID, lease.JobLease.AgentAssignmentID, lease.JobLease.ExecutionEpoch = turnID, id, participant, 1
	ref := execution.StateRef{ID: participant, ParticipantID: participant, SessionID: id, Backend: execution.BackendBinding{Family: "diagnostic", Version: 1, Namespace: uuid.NewString()}, Format: "pi-session/v3", RuntimeProfileSHA256: "diagnostic", Image: "diagnostic"}
	connection := &timedDiagnosticConnection{deadline: time.Now().Add(50 * time.Millisecond)}
	connection.Connection = diagnosticConnectionID{id: id}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := NewRuntimeCoordinator(blockingDiagnosticBinding{&diagnosticBindingStore{session: s}}).Prepare(ctx, RuntimePrepareRequest{Session: s, State: ref, Lease: lease, Connect: func(context.Context, agent.SessionIdentity) (agent.Connection, error) { return connection, nil }})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(connection.completed, context.DeadlineExceeded) || !connection.closed || ctx.Err() != nil {
		t.Fatalf("durable binding exceeded adapter deadline or connection remained open: %v, completed=%v, closed=%t, parent=%v", err, connection.completed, connection.closed, ctx.Err())
	}
}

type diagnosticConnectionID struct {
	agent.Connection
	id string
}

func (c diagnosticConnectionID) PreparedSessionID() string { return c.id }
