//go:build integration

package store_test

import (
	"context"
	"errors"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRetainedStateBindingsPersistAndFenceWriters(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding := execution.BackendBinding{Family: "docker", Version: 1, Namespace: "retained-state-fixture"}
	if err := databases[0].BindExecutionBackend(ctx, binding); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []execution.BackendBinding{
		{Family: "microsandbox", Version: 1, Namespace: binding.Namespace},
		{Family: "docker", Version: 2, Namespace: binding.Namespace},
		{Family: "docker", Version: 1, Namespace: "another-volume"},
	} {
		if err := databases[1].BindExecutionBackend(ctx, mismatch); !errors.Is(err, execution.ErrBindingMismatch) {
			t.Fatalf("backend mismatch = %v", err)
		}
	}
	runtimeProfile, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 993, workflow.RoleDeveloper, runtimeProfile.Binding(), runtimeProfile.Binding())
	spec := fixture.turnSpec()
	spec.AgentProfileConfig = agentProfileConfig("developer", workflow.RoleDeveloper, "pi-rpc/v2", "provider/test", "", 0, "Test instructions.", nil)
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireFixtureAgentTurn(t, databases[0], pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "state-writer", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := databases[0].AgentTurnState(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != fixture.assignmentID || ref.SessionID != fixture.sessionID || ref.Backend != binding || ref.RuntimeProfileSHA256 != runtimeProfile.ContentSHA256() {
		t.Fatalf("bound reference = %#v", ref)
	}
	otherRef, err := databases[1].AgentTurnState(ctx, lease)
	if err != nil || otherRef != ref {
		t.Fatalf("reopened binding = %#v, %v", otherRef, err)
	}
	root := t.TempDir()
	states, err := docker.NewRetainedState(root, binding.Namespace, databases[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].MarkAgentTurnStateInitialized(ctx, lease, ref); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "assignment-"+ref.ID, "runtime-state")); err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("missing protected state reset = %v", err)
	}
	wrongSession := ref
	wrongSession.SessionID = "20000000-0000-4000-8000-999999999999"
	if _, _, err := databases[0].StateMetadata(ctx, wrongSession); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("foreign Session attachment = %v", err)
	}
	if _, err := databases[0].CollectionState(ctx, ref.ID); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("active collection = %v", err)
	}
	if err := databases[0].WithStateRecovery(ctx, ref.ID, func(execution.StateRef, execution.StateMetadataSource) error {
		t.Fatal("active state exported")
		return nil
	}); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("active recovery = %v", err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err := databases[1].AgentTurnState(ctx, lease); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale writer = %v", err)
	}
	if err := databases[1].MarkAgentTurnStateInitialized(ctx, lease, ref); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale initialization = %v", err)
	}
}

func TestRetainedStateOfflineRecoveryExcludesCollectionAndConcurrentWriters(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding := execution.BackendBinding{Family: "docker", Version: 1, Namespace: "retained-state-fixture"}
	if err := databases[0].BindExecutionBackend(ctx, binding); err != nil {
		t.Fatal(err)
	}
	fixture := seedAgentSession(t, pool, 994)
	// Offline recovery can read a protected Session without taking a Turn fence.
	if err := databases[0].WithStateRecovery(ctx, fixture.assignmentID, func(ref execution.StateRef, metadata execution.StateMetadataSource) error {
		if ref.ParticipantID != fixture.assignmentID || ref.SessionID != "" {
			t.Fatalf("recovery reference = %#v", ref)
		}
		if _, _, err := metadata.StateMetadata(ctx, ref); err != nil {
			return err
		}
		blockedCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := databases[1].BindExecutionBackend(blockedCtx, binding); err == nil {
			t.Fatal("recovery did not exclude backend admission")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := databases[1].BindExecutionBackend(ctx, binding); err != nil {
		t.Fatalf("recovery exclusion retained after completion: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE retained_runtime_states SET metadata = '{"version":2}' WHERE agent_participant_id = $1`, fixture.assignmentID); err == nil {
		t.Fatal("state location was mutable")
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_backend_binding SET namespace = 'replacement'`); err == nil {
		t.Fatal("backend namespace was mutable")
	}
}

func TestRetainedStateCollectionKeepsNeighborProtectedAndReadinessAvailable(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding := execution.BackendBinding{Family: "docker", Version: 1, Namespace: "collection-state-fixture"}
	if err := database.BindExecutionBackend(ctx, binding); err != nil {
		t.Fatal(err)
	}
	runtimeProfile, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 996, workflow.RoleDeveloper, runtimeProfile.Binding(), runtimeProfile.Binding())
	neighbor := seedAgentSessionWithRuntimeBindings(t, pool, 997, workflow.RoleDeveloper, runtimeProfile.Binding(), runtimeProfile.Binding())
	root := t.TempDir()
	states, err := docker.NewRetainedState(root, binding.Namespace, database)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := database.ListProtectedStates(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("protected states = %v, %v", refs, err)
	}
	for _, ref := range refs {
		if err := states.Ensure(ctx, ref); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE retained_runtime_states SET initialized = TRUE WHERE agent_participant_id = $1`, ref.ID); err != nil {
			t.Fatal(err)
		}
	}
	prepareClosableFixture(t, pool, fixture)
	observed := time.Now().UTC().Add(-time.Hour)
	closeAndSettleWithoutTurn(t, database, pool, ctx, fixture, 996, "71000000-0000-4000-8000-000000009961", "state-collection", "state-collection-retention", observed, observed.Add(time.Minute))
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "state-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("collection lease = %v, %v", lease, err)
	}
	authorization, err := database.AuthorizeAssignmentCollection(ctx, *lease)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := database.CollectionState(ctx, fixture.assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := states.Collect(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	// Physical collection precedes the durable acknowledgement and tool cleanup.
	// It must not make unrelated Work Items fail protected-state readiness.
	if err := states.Check(ctx); err != nil {
		t.Fatalf("authorized collection broke readiness: %v", err)
	}
	protected, err := database.ListProtectedStates(ctx)
	if err != nil || len(protected) != 1 || protected[0].ID != neighbor.assignmentID {
		t.Fatalf("neighbor protection = %v, %v", protected, err)
	}
	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CollectionState(ctx, fixture.assignmentID); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("stale collection authority = %v", err)
	}
}
