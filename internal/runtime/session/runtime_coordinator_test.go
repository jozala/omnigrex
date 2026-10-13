package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/store"
)

func TestRuntimeCoordinatorHumanPromptDeniesSubmissionWhenFenceLostDuringReplay(t *testing.T) {
	for _, loss := range []string{"lease token", "control revision"} {
		t.Run(loss, func(t *testing.T) {
			s := runtimeHumanSession()
			ledger := &diagnosticBindingStore{session: s}
			connection := &humanPromptConnection{session: s}
			connection.replay = func() {
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if ledger.token == "" {
					t.Fatal("history replay started without durable admission")
				}
				switch loss {
				case "lease token":
					ledger.token = "replacement-token"
				case "control revision":
					ledger.session.ControlRevision++
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			outcome, err := NewRuntimeCoordinator(ledger).HumanPrompt(ctx, runtimeHumanRequest(t, s, connection))
			if !errors.Is(err, store.ErrHumanPromptLeaseLost) {
				t.Fatalf("HumanPrompt error = %v, want lost replay fence", err)
			}
			if outcome.Status != agent.Rejected || outcome.Admitted || connection.promptCalls != 0 {
				t.Fatalf("prompt submitted after replay lost its fence: outcome=%#v, prompts=%d", outcome, connection.promptCalls)
			}
			if connection.replayCalls != 1 || connection.closeCalls == 0 {
				t.Fatalf("replay or connection cleanup missing: replay=%d, close=%d", connection.replayCalls, connection.closeCalls)
			}
		})
	}
}

func TestRuntimeCoordinatorHumanPromptCompletedReleaseFailureIsUncertain(t *testing.T) {
	s := runtimeHumanSession()
	releaseErr := errors.New("exact human prompt lease release failed")
	ledger := &releaseFailingHumanStore{diagnosticBindingStore: &diagnosticBindingStore{session: s}, releaseErr: releaseErr}
	connection := &humanPromptConnection{session: s}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	outcome, err := NewRuntimeCoordinator(ledger).HumanPrompt(ctx, runtimeHumanRequest(t, s, connection))
	if !errors.Is(err, ErrHumanPromptOutcomeUncertain) || !errors.Is(err, releaseErr) {
		t.Fatalf("HumanPrompt error = %v, want uncertainty and exact release error", err)
	}
	if outcome.Status != agent.Completed || !outcome.Admitted || connection.promptCalls != 1 || connection.replayCalls != 1 {
		t.Fatalf("completed prompt evidence lost: outcome=%#v, prompts=%d, replay=%d", outcome, connection.promptCalls, connection.replayCalls)
	}
	if ledger.acquired.Token == "" || !reflect.DeepEqual(ledger.released, ledger.acquired) || ledger.releaseCalls != 1 {
		t.Fatalf("release did not target exact admission: acquired=%#v, released=%#v, calls=%d", ledger.acquired, ledger.released, ledger.releaseCalls)
	}
	if ledger.releaseContextErr != nil || connection.closeCalls == 0 {
		t.Fatalf("release or cleanup invalid: context=%v, close=%d", ledger.releaseContextErr, connection.closeCalls)
	}
}

func runtimeHumanSession() store.AgentSession {
	return store.AgentSession{
		ID: "human-session", AgentAssignmentID: "participant", RuntimeSessionID: "runtime-session",
		Status: store.AgentSessionRetained, ControlOwner: store.SessionControlHuman, ControlRevision: 7,
		RuntimeCapabilities: json.RawMessage(`{"history":true}`),
	}
}

func runtimeHumanRequest(t *testing.T, s store.AgentSession, connection agent.Connection) RuntimeHumanPromptRequest {
	t.Helper()
	return RuntimeHumanPromptRequest{
		Session: s, Text: "continue after replay", Sink: agentevent.NoopSink{},
		Connect: func(_ context.Context, identity agent.SessionIdentity) (agent.Connection, error) {
			if identity != (agent.SessionIdentity{AgentSessionID: s.ID, RuntimeSessionID: s.RuntimeSessionID, MustExist: true}) {
				t.Fatalf("reopened wrong Session: %#v", identity)
			}
			return connection, nil
		},
	}
}

type humanPromptConnection struct {
	session                              store.AgentSession
	replay                               func()
	replayCalls, promptCalls, closeCalls int
}

func (c *humanPromptConnection) Prepare(context.Context) (agent.Ready, error) {
	return agent.Ready{RuntimeSessionID: c.session.RuntimeSessionID, Capabilities: c.session.RuntimeCapabilities}, nil
}
func (c *humanPromptConnection) PreparedSessionID() string { return c.session.RuntimeSessionID }
func (c *humanPromptConnection) Replay(_ context.Context, _ agent.HistoryCursor, _ agentevent.Sink) (agent.HistoryCursor, error) {
	c.replayCalls++
	if c.replay != nil {
		c.replay()
	}
	return agent.HistoryCursor{RuntimeSessionID: c.session.RuntimeSessionID, EntryID: "last-history-entry"}, nil
}
func (*humanPromptConnection) SetAgentEventContext(agentevent.Context) {}
func (c *humanPromptConnection) Prompt(context.Context, string, agentevent.Sink) (agent.Outcome, error) {
	c.promptCalls++
	return agent.Outcome{Admitted: true, Status: agent.Completed}, nil
}
func (*humanPromptConnection) Cancel(context.Context) error { return nil }
func (c *humanPromptConnection) Close() error               { c.closeCalls++; return nil }

type releaseFailingHumanStore struct {
	*diagnosticBindingStore
	releaseErr, releaseContextErr error
	acquired, released            store.HumanPromptLease
	releaseCalls                  int
}

func (d *releaseFailingHumanStore) AcquireHumanPromptLease(ctx context.Context, id string, revision int64, runtimeID string, duration time.Duration) (store.HumanPromptLease, error) {
	lease, err := d.diagnosticBindingStore.AcquireHumanPromptLease(ctx, id, revision, runtimeID, duration)
	d.acquired = lease
	return lease, err
}
func (d *releaseFailingHumanStore) ReleaseHumanPromptLease(ctx context.Context, lease store.HumanPromptLease) error {
	d.released, d.releaseContextErr = lease, ctx.Err()
	d.releaseCalls++
	return d.releaseErr
}
