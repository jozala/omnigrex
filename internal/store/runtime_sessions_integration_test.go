//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
)

func TestDurableRuntimeSessionReservationAndActivationAreFenced(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_ = triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000001011", "62000000-0000-4000-8000-000000001011")
	spec := preparationSpec("pi-profile", "diagnostic/controlled")
	spec.Developer.Binding.RuntimeProfileName = "pi-rpc"
	spec.Developer.Binding.RuntimeProfileVersion = "v1"
	spec.Developer.Profile.Config = json.RawMessage(`{"name":"developer","path":".omnigrex/team/developer.md","role":"DEVELOPER","runtime":"pi-rpc/v1","model":"diagnostic/controlled","thinking":"off","permissions":{"read":"allow"},"instructions":"Perform development."}`)
	prepared, err := database.PrepareAgentTurn(ctx, claimPreparationJob(t, database, ctx), spec)
	if err != nil {
		t.Fatal(err)
	}
	job := agentTurnExecutionJob(t, pool, ctx, prepared.Turn)
	lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, job, prepared.Turn.ControlRevision, "pi-runtime", 10*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BindExecutionBackend(ctx, execution.BackendBinding{Family: "docker", Version: 1, Namespace: "pi-test"}); err != nil {
		t.Fatal(err)
	}
	ref, err := database.AgentTurnState(ctx, lease)
	if err != nil || ref.Format != "pi-session/v3" {
		t.Fatalf("state binding: %+v %v", ref, err)
	}
	reserved, err := database.ReserveAgentSessionRuntime(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.RuntimeSessionID != reserved.ID || reserved.Status != store.AgentSessionCreating {
		t.Fatalf("reservation: %+v", reserved)
	}
	stale := lease
	stale.OwnerToken = "65000000-0000-4000-8000-000000001011"
	if _, err := database.ReserveAgentSessionRuntime(ctx, stale); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale reservation: %v", err)
	}
	ready := agent.Ready{RuntimeSessionID: reserved.ID, Capabilities: json.RawMessage(`{"history_replay":true,"session_continuation":true,"state_format":"pi-session/v3"}`)}
	wrong := ready
	wrong.RuntimeSessionID = "wrong-session"
	if _, err := database.BindAgentSessionRuntime(ctx, lease, wrong); !errors.Is(err, store.ErrAgentSessionRuntimeConflict) {
		t.Fatalf("wrong identity: %v", err)
	}
	bound, err := database.BindAgentSessionRuntime(ctx, lease, ready)
	if err != nil || bound.Status != store.AgentSessionActive || string(bound.RuntimeCapabilities) != string(ready.Capabilities) {
		t.Fatalf("activation: %+v %v", bound, err)
	}
	if err := database.ValidateRuntimeSessionPromptFence(ctx, lease, wrong.RuntimeSessionID); !errors.Is(err, store.ErrAgentSessionRuntimeConflict) {
		t.Fatalf("wrong prompt identity: %v", err)
	}
	if err := database.ValidateRuntimeSessionPromptFence(ctx, lease, bound.RuntimeSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BindAgentSessionRuntime(ctx, stale, ready); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale activation: %v", err)
	}
	client := &runtimeSessionClient{ready: ready, outcome: agent.Outcome{Admitted: true, Status: agent.Completed}}
	coordinator := session.NewRuntimeCoordinator(database)
	result, err := coordinator.Prepare(ctx, session.RuntimePrepareRequest{Lease: lease, Session: bound, State: ref, Connect: func(_ context.Context, identity agent.SessionIdentity) (agent.Connection, error) {
		if identity.RuntimeSessionID != bound.ID || !identity.MustExist {
			t.Fatalf("reopening identity: %+v", identity)
		}
		return client, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Prompt(ctx, session.RuntimePromptRequest{Lease: stale, Session: result.Session, Connection: client, Text: "must-not-run"}); !errors.Is(err, store.ErrAgentTurnFenceLost) || client.prompts != 0 {
		t.Fatalf("stale prompt: %v", err)
	}
	other := &runtimeSessionClient{ready: agent.Ready{RuntimeSessionID: "another-session"}}
	if _, err := coordinator.Prompt(ctx, session.RuntimePromptRequest{Lease: lease, Session: result.Session, Connection: other, Text: "must-not-run"}); !errors.Is(err, agent.ErrSession) || other.prompts != 0 {
		t.Fatalf("wrong live connection: %v", err)
	}
	if outcome, err := coordinator.Prompt(ctx, session.RuntimePromptRequest{Lease: lease, Session: result.Session, Connection: client, Text: "allowed"}); err != nil || outcome.Status != agent.Completed || client.prompts != 1 {
		t.Fatalf("prompt: %+v %v", outcome, err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed}); err != nil {
		t.Fatal(err)
	}
	human, err := database.TransferAgentSessionControl(ctx, bound.ID, bound.ControlRevision, store.SessionControlHuman, "human")
	if err != nil {
		t.Fatal(err)
	}
	client = &runtimeSessionClient{ready: ready, replayErr: agent.ErrProtocol, outcome: agent.Outcome{Status: agent.Completed, Admitted: true}}
	humanRequest := session.RuntimeHumanPromptRequest{Session: human, Text: "human followup", Sink: agentevent.NoopSink{}, Connect: func(_ context.Context, identity agent.SessionIdentity) (agent.Connection, error) {
		if !identity.MustExist || identity.RuntimeSessionID != human.ID {
			t.Fatalf("human reopen: %+v", identity)
		}
		return client, nil
	}}
	if _, err := coordinator.HumanPrompt(ctx, humanRequest); !errors.Is(err, agent.ErrProtocol) || client.prompts != 0 || !client.closed {
		t.Fatalf("failed replay admitted human prompt: %v", err)
	}
	client = &runtimeSessionClient{ready: agent.Ready{RuntimeSessionID: human.RuntimeSessionID, Capabilities: json.RawMessage(`{"history_replay":false}`)}}
	if _, err := coordinator.HumanPrompt(ctx, humanRequest); !errors.Is(err, session.ErrCapabilityMismatch) || client.replayed || client.prompts != 0 {
		t.Fatalf("changed retained capabilities: %v", err)
	}
	client = &runtimeSessionClient{ready: ready, outcome: agent.Outcome{Status: agent.Completed, Admitted: true}}
	if outcome, err := coordinator.HumanPrompt(ctx, humanRequest); err != nil || outcome.Status != agent.Completed || !client.replayed || client.prompts != 1 {
		t.Fatalf("human prompt: %+v %v", outcome, err)
	}
	client = &runtimeSessionClient{ready: ready, outcome: agent.Outcome{Status: agent.Unknown, Admitted: true}}
	if _, err := coordinator.HumanPrompt(ctx, humanRequest); !errors.Is(err, session.ErrHumanPromptOutcomeUncertain) {
		t.Fatalf("unknown human prompt: %v", err)
	}
	if _, err := database.AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.RuntimeSessionID, time.Second); !errors.Is(err, store.ErrHumanPromptLeaseActive) {
		t.Fatalf("uncertain prompt lost writer barrier: %v", err)
	}
}

type runtimeSessionClient struct {
	ready     agent.Ready
	outcome   agent.Outcome
	prompts   int
	closed    bool
	replayErr error
	replayed  bool
}

func (c *runtimeSessionClient) Prepare(context.Context) (agent.Ready, error) { return c.ready, nil }
func (c *runtimeSessionClient) PreparedSessionID() string                    { return c.ready.RuntimeSessionID }
func (c *runtimeSessionClient) Prompt(context.Context, string, agentevent.Sink) (agent.Outcome, error) {
	c.prompts++
	return c.outcome, nil
}
func (c *runtimeSessionClient) Replay(_ context.Context, cursor agent.HistoryCursor, _ agentevent.Sink) (agent.HistoryCursor, error) {
	c.replayed = true
	return cursor, c.replayErr
}
func (*runtimeSessionClient) SetAgentEventContext(agentevent.Context) {}
func (*runtimeSessionClient) Cancel(context.Context) error            { return nil }
func (c *runtimeSessionClient) Close() error                          { c.closed = true; return nil }
