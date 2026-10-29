//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

func TestSnapshotInstallationCannotSucceedAfterControlLoss(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1802)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := agentTurnExecutionJob(t, pool, ctx, turn)
	lease, err := acquireFixtureAgentTurn(t, databases[0], pool, ctx, job, turn.ControlRevision, "runtime", 3*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: lease.AgentParticipantID, AgentSessionID: lease.AgentSessionID, AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
	if _, _, err := databases[0].ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	called := false
	apply := func(context.Context) error { called = true; return nil }
	if err := databases[0].WithSnapshotInstallationFence(ctx, lease, key, apply); !errors.Is(err, store.ErrAgentTurnFenceLost) || called {
		t.Fatal("unsettled execution admitted", err)
	}
	if err := databases[0].BindExecution(ctx, key, "fixture-runtime"); err != nil {
		t.Fatal(err)
	}
	err = databases[0].WithSnapshotInstallationFence(ctx, lease, key, func(context.Context) error {
		called = true
		// Control can change without waiting behind the physical execution guard.
		// Its cleanup must still revoke the reservation before replacing writers.
		_, err := pool.Exec(ctx, `UPDATE agent_sessions SET control_revision=control_revision+1 WHERE id=$1`, lease.AgentSessionID)
		return err
	})
	if !errors.Is(err, store.ErrAgentTurnFenceLost) || !called {
		t.Fatal("lost control became successful installation evidence", err)
	}
	called = false
	if err := databases[0].WithSnapshotInstallationFence(ctx, lease, key, apply); !errors.Is(err, store.ErrAgentTurnFenceLost) || called {
		t.Fatal("stale authority admitted", err)
	}
	// All failure paths release the cleanup guard.
	if _, _, err := databases[0].RevokeExecution(ctx, key); err != nil {
		t.Fatal("failed installation stranded cleanup", err)
	}
}

func TestSnapshotInstallationKeepsHeartbeatsLive(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 1801)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := agentTurnExecutionJob(t, pool, ctx, turn)
	lease, err := acquireFixtureAgentTurn(t, databases[0], pool, ctx, job, turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: lease.AgentParticipantID, AgentSessionID: lease.AgentSessionID, AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
	if _, _, err := databases[0].ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].BindExecution(ctx, key, "fixture-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "snapshot", ToolName: "refresh_default_branch", Request: json.RawMessage(`{"operation_id":"snapshot"}`), ExternalService: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*execution.Key){func(k *execution.Key) { k.AgentSessionID = "10000000-0000-4000-8000-000000000099" }, func(k *execution.Key) { k.AssignmentID = "10000000-0000-4000-8000-000000000099" }, func(k *execution.Key) { k.ExecutionEpoch++ }, func(k *execution.Key) { k.AgentTurnID = "10000000-0000-4000-8000-000000000099" }, func(k *execution.Key) { k.RuntimeProfileVersion = "other" }} {
		wrong := key
		change(&wrong)
		called := false
		if err := databases[0].WithSnapshotInstallationFence(ctx, lease, wrong, func(context.Context) error { called = true; return nil }); !errors.Is(err, store.ErrAgentTurnFenceLost) || called {
			t.Fatal("wrong execution admitted", wrong, err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	installed := make(chan error, 1)
	go func() {
		installed <- databases[0].WithSnapshotInstallationFence(ctx, lease, key, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	heartbeatCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	refreshed, err := databases[1].RefreshAgentTurnLease(heartbeatCtx, lease, 3*time.Second)
	if err != nil {
		t.Fatalf("heartbeat blocked during installation: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := databases[1].ValidateTurnFence(ctx, refreshed); err != nil {
		t.Fatal("slow installation lost authority", err)
	}
	revoked := make(chan error, 1)
	go func() { _, _, err := databases[1].RevokeExecution(ctx, key); revoked <- err }()
	select {
	case err := <-revoked:
		t.Fatalf("cleanup passed live installation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	queuedCtx, queuedCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer queuedCancel()
	if _, err := databases[1].RefreshAgentTurnLease(queuedCtx, refreshed, 3*time.Second); err != nil {
		t.Fatal("queued cleanup blocked heartbeat", err)
	}
	close(release)
	if err := <-installed; err != nil {
		t.Fatal("slow installation failed", err)
	}
	if err := databases[0].CompleteMutation(ctx, refreshed, mutation.ID, json.RawMessage(`{"status":"current"}`)); err != nil {
		t.Fatal("installation result could not be recorded", err)
	}
	if err := <-revoked; err != nil {
		t.Fatal("cleanup did not resume", err)
	}
	called := false
	if err := databases[0].WithSnapshotInstallationFence(ctx, refreshed, key, func(context.Context) error { called = true; return nil }); !errors.Is(err, store.ErrAgentTurnFenceLost) || called {
		t.Fatal("revoked execution was admitted", err)
	}
}
