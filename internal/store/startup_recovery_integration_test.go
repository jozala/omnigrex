//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
)

func TestStartupRecoveryClassifiesAndRecoversExactRuntimeIdentity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	fixture := seedAgentSession(t, pool, 101)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}

	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	identity := store.AgentTurnRuntimeIdentity{
		AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID,
		AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch,
		RuntimeProfileName: "runtime", RuntimeProfileVersion: "1",
	}
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeActive)

	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	lease, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "startup-runtime", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeLive)
	if _, found, err := databases[0].ClassifyAgentTurnRuntime(ctx, store.AgentTurnRuntimeIdentity{
		AssignmentID: identity.AssignmentID, AgentSessionID: identity.AgentSessionID,
		AgentTurnID: identity.AgentTurnID, ExecutionEpoch: identity.ExecutionEpoch,
		RuntimeProfileName: "other", RuntimeProfileVersion: "1",
	}); err != nil || found {
		t.Fatalf("ClassifyAgentTurnRuntime() profile mismatch = (found %t, %v)", found, err)
	}

	if err := databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	inFlight, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "startup-in-flight", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := databases[0].StartMutation(ctx, lease, inFlight.ID); err != nil {
		t.Fatal(err)
	}
	reserved, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "startup-reserved", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, job.ID, turn.ID)
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeExpired)

	results := make(chan bool, 2)
	errorsFound := make(chan error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, database := range databases {
		wait.Add(1)
		go func(database *store.Store) {
			defer wait.Done()
			<-start
			_, recovered, err := database.ClaimAndRecoverExpiredAgentTurn(ctx)
			results <- recovered
			errorsFound <- err
		}(database)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	recoveredCount := 0
	for recovered := range results {
		if recovered {
			recoveredCount++
		}
	}
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("ClaimAndRecoverExpiredAgentTurn() error = %v", err)
		}
	}
	if recoveredCount != 1 {
		t.Fatalf("claimed recoveries = %d, want 1", recoveredCount)
	}
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeRecovery)
	if _, recovered, err := databases[0].ClaimAndRecoverExpiredAgentTurn(ctx); err != nil || recovered {
		t.Fatalf("fixed-point ClaimAndRecoverExpiredAgentTurn() = (%t, %v)", recovered, err)
	}

	recovery, err := databases[0].RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		t.Fatalf("idempotent RecoverExpiredAgentTurn() error = %v", err)
	}
	for range 4 {
		repeated, err := databases[1].RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
		if err != nil || repeated.StopRuntimeJobID != recovery.StopRuntimeJobID ||
			repeated.ReconcileMutationsJobID != recovery.ReconcileMutationsJobID {
			t.Fatalf("repeated RecoverExpiredAgentTurn() = (%#v, %v), want original barrier", repeated, err)
		}
	}
	var inFlightState, reservedState string
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, inFlight.ID).Scan(&inFlightState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, reserved.ID).Scan(&reservedState); err != nil {
		t.Fatal(err)
	}
	if inFlightState != string(store.MutationUnknown) || reservedState != string(store.MutationFailed) {
		t.Fatalf("recovered mutation states = in-flight %s, reserved %s", inFlightState, reservedState)
	}

	states, err := databases[0].ListAgentTurnRuntimeStates(ctx, "", 1)
	if err != nil || len(states) != 1 || states[0].Identity != identity || states[0].Disposition != store.AgentTurnRuntimeRecovery {
		t.Fatalf("ListAgentTurnRuntimeStates() = (%#v, %v)", states, err)
	}
	if _, err := databases[0].ListAgentTurnRuntimeStates(ctx, "not-a-uuid", 1); err == nil {
		t.Fatal("ListAgentTurnRuntimeStates() invalid cursor error = nil")
	}
}

func TestStartupReconcilerFencesLiveDuplicateRuntimeProcessesBeforeCleanup(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 105)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', state_revision = 1 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}

	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, 5*time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "duplicate-live-runtime", 5*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	inFlight, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "duplicate-live-in-flight", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, inFlight.ID); err != nil {
		t.Fatal(err)
	}
	reserved, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "duplicate-live-reserved", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := runtimeIdentityForFixture(fixture, turn)
	assertRuntimeDisposition(t, database, ctx, identity, store.AgentTurnRuntimeLive)
	runtimes := &duplicateStartupRuntimes{
		database: database,
		duplicate: startup.DuplicateManagedRuntimeProcess{
			Identity: identity, ContainerIDs: []string{"duplicate-live-a", "duplicate-live-b"},
		},
	}
	reconciler, err := startup.NewReconciler(database, runtimes, runtimes, runtimes, startup.ReconcilerOptions{
		MaxPasses: 10, MaxRuntimeProcesses: 10, MaxRecoveries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := reconciler.Reconcile(ctx)
	if err != nil || result.Passes != 3 || result.RemovedRuntimeIdentities != 1 || runtimes.cleanupCalls != 1 || !runtimes.absent {
		t.Fatalf("Reconcile() live duplicates = (%#v, %v), cleanup calls %d absent %t", result, err, runtimes.cleanupCalls, runtimes.absent)
	}
	assertRuntimeDisposition(t, database, ctx, identity, store.AgentTurnRuntimeRecovery)
	recovery, err := database.GetAgentTurnRecovery(ctx, turn.ID, turn.ExecutionEpoch)
	if err != nil || recovery.ExecutionEpoch != turn.ExecutionEpoch || recovery.SuccessorAllowed ||
		recovery.StopRuntimeJobID == "" || recovery.ReconcileMutationsJobID == "" {
		t.Fatalf("GetAgentTurnRecovery() = (%#v, %v), want unsettled same-epoch recovery", recovery, err)
	}
	repeated, err := database.FenceDuplicateAgentTurnRuntime(ctx, identity)
	if err != nil || repeated.StopRuntimeJobID != recovery.StopRuntimeJobID ||
		repeated.ReconcileMutationsJobID != recovery.ReconcileMutationsJobID {
		t.Fatalf("repeated FenceDuplicateAgentTurnRuntime() = (%#v, %v), want original barrier", repeated, err)
	}

	var admissionOpen, admissionClosed, ownerCleared bool
	var nextExecutionEpoch int64
	if err := pool.QueryRow(ctx, `
SELECT turn.mutation_admission_open, turn.mutation_admission_closed_at IS NOT NULL,
       turn.owner_id IS NULL AND turn.owner_token IS NULL, session.next_execution_epoch
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
WHERE turn.id = $1`, turn.ID).Scan(&admissionOpen, &admissionClosed, &ownerCleared, &nextExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	var slots int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turn_slots WHERE agent_turn_id = $1`, turn.ID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	var inFlightState, reservedState string
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, inFlight.ID).Scan(&inFlightState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id = $1`, reserved.ID).Scan(&reservedState); err != nil {
		t.Fatal(err)
	}
	if admissionOpen || !admissionClosed || !ownerCleared || nextExecutionEpoch != turn.ExecutionEpoch+1 || slots != 0 ||
		inFlightState != string(store.MutationUnknown) || reservedState != string(store.MutationFailed) {
		t.Errorf("duplicate fence = admission %t/%t owner-cleared %t next-epoch %d slots %d mutations %s/%s",
			admissionOpen, admissionClosed, ownerCleared, nextExecutionEpoch, slots, inFlightState, reservedState)
	}
	if _, err := database.AllocateAgentTurn(ctx, fixture.turnSpec()); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Errorf("AllocateAgentTurn() during duplicate recovery error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
}

func TestClassifyAgentTurnRuntimeRequiresCompleteLiveAuthorityChain(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string, agentFixture, store.AgentTurn, store.AgentTurnLease)
		want   store.AgentTurnRuntimeDisposition
	}{
		{
			name: "missing execution job",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `DELETE FROM job_attempts WHERE job_id = $1`, lease.JobLease.ID)
				execRuntimeAuthoritySQL(t, pool, ctx, `DELETE FROM jobs WHERE id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "terminal execution job",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE, last_error = 'test terminal state'
WHERE job_id = $1`, lease.JobLease.ID)
				execRuntimeAuthoritySQL(t, pool, ctx, `
UPDATE jobs
SET status = 'FAILED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, completed_at = clock_timestamp(),
    last_error = 'test terminal state'
WHERE id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeTerminal,
		},
		{
			name: "missing live job attempt",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `DELETE FROM job_attempts WHERE job_id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "job and attempt lease token mismatch",
			mutate: func(t *testing.T, token string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE job_attempts SET lease_token = $2 WHERE job_id = $1`, lease.JobLease.ID, token)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "job and attempt owner mismatch",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE job_attempts SET lease_owner = 'other-owner' WHERE job_id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "terminal job attempt",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE, last_error = 'test terminal attempt'
WHERE job_id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "missing turn slot",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `DELETE FROM agent_turn_slots WHERE agent_turn_id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "turn and slot owner mismatch",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turn_slots SET owner_id = 'other-owner' WHERE agent_turn_id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "turn and slot owner token mismatch",
			mutate: func(t *testing.T, token string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turn_slots SET owner_token = $2 WHERE agent_turn_id = $1`, turn.ID, token)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "turn and slot control mismatch",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turn_slots SET control_revision = control_revision + 1 WHERE agent_turn_id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "job payload identity mismatch",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `
UPDATE jobs SET payload = jsonb_set(payload, '{control_revision}', '999'::jsonb)
WHERE id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "turn lifecycle state mismatch",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turns SET status = 'STARTING' WHERE id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "session control revision mismatch",
			mutate: func(t *testing.T, _ string, fixture agentFixture, _ store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_sessions SET control_revision = control_revision + 1 WHERE id = $1`, fixture.sessionID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "only job lease expired",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "only attempt lease expired",
			mutate: func(t *testing.T, _ string, _ agentFixture, _ store.AgentTurn, lease store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1`, lease.JobLease.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "only turn lease expired",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
		{
			name: "only slot lease expired",
			mutate: func(t *testing.T, _ string, _ agentFixture, turn store.AgentTurn, _ store.AgentTurnLease) {
				execRuntimeAuthoritySQL(t, pool, ctx, `UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, turn.ID)
			},
			want: store.AgentTurnRuntimeActive,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedAgentSession(t, pool, 110+index)
			turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			job := claimAgentTurnJob(t, database, ctx, turn, time.Minute)
			lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "runtime-owner", time.Minute, 100)
			if err != nil {
				t.Fatal(err)
			}
			identity := runtimeIdentityForFixture(fixture, turn)
			assertRuntimeDisposition(t, database, ctx, identity, store.AgentTurnRuntimeLive)

			token := fmt.Sprintf("79000000-0000-4000-8000-%012d", index+1)
			test.mutate(t, token, fixture, turn, lease)
			assertRuntimeDisposition(t, database, ctx, identity, test.want)
		})
	}
}

func TestClassifyAgentTurnRuntimeObservesConcurrentLeaseChangeAtomically(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 130)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, time.Minute)
	if _, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "concurrent-owner", time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	identity := runtimeIdentityForFixture(fixture, turn)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, update := range []struct {
		query string
		id    string
	}{
		{`UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, job.ID},
		{`UPDATE job_attempts SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE job_id = $1`, job.ID},
		{`UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, turn.ID},
		{`UPDATE agent_turn_slots SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE agent_turn_id = $1`, turn.ID},
	} {
		if _, err := tx.Exec(ctx, update.query, update.id); err != nil {
			t.Fatal(err)
		}
	}

	const readers = 8
	results := make(chan store.AgentTurnRuntimeDisposition, readers)
	errorsFound := make(chan error, readers)
	var wait sync.WaitGroup
	for range readers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			state, found, err := database.ClassifyAgentTurnRuntime(ctx, identity)
			if err == nil && !found {
				err = errors.New("durable Runtime Process identity not found")
			}
			results <- state.Disposition
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	for disposition := range results {
		if disposition != store.AgentTurnRuntimeLive {
			t.Fatalf("classification during uncommitted lease change = %s, want LIVE", disposition)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertRuntimeDisposition(t, database, ctx, identity, store.AgentTurnRuntimeExpired)
}

func execRuntimeAuthoritySQL(t *testing.T, pool *pgxpool.Pool, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func runtimeIdentityForFixture(fixture agentFixture, turn store.AgentTurn) store.AgentTurnRuntimeIdentity {
	return store.AgentTurnRuntimeIdentity{
		AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID,
		AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch,
		RuntimeProfileName: "runtime", RuntimeProfileVersion: "1",
	}
}

func TestStartupRecoveryExactCallsAreIdempotentAcrossStores(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	fixture := seedAgentSession(t, pool, 102)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "startup-runtime", time.Second, 1); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, job.ID, turn.ID)

	start := make(chan struct{})
	recoveries := make(chan store.AgentTurnRecovery, len(databases))
	errorsFound := make(chan error, len(databases))
	var wait sync.WaitGroup
	for _, database := range databases {
		wait.Add(1)
		go func(database *store.Store) {
			defer wait.Done()
			<-start
			recovery, err := database.RecoverExpiredAgentTurn(ctx, turn.ID, turn.ExecutionEpoch)
			recoveries <- recovery
			errorsFound <- err
		}(database)
	}
	close(start)
	wait.Wait()
	close(recoveries)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent RecoverExpiredAgentTurn() error = %v", err)
		}
	}
	var first store.AgentTurnRecovery
	for recovery := range recoveries {
		if first.TurnID == "" {
			first = recovery
		} else if recovery.StopRuntimeJobID != first.StopRuntimeJobID || recovery.ReconcileMutationsJobID != first.ReconcileMutationsJobID {
			t.Fatalf("concurrent recovery barriers differ: %#v and %#v", first, recovery)
		}
	}
	var stopJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE agent_turn_id = $1 AND kind = 'STOP_STALE_RUNTIME'`, turn.ID).Scan(&stopJobs); err != nil {
		t.Fatal(err)
	}
	if stopJobs != 1 {
		t.Fatalf("STOP_STALE_RUNTIME jobs = %d, want 1", stopJobs)
	}
}

func assertRuntimeDisposition(t *testing.T, database *store.Store, ctx context.Context, identity store.AgentTurnRuntimeIdentity, want store.AgentTurnRuntimeDisposition) {
	t.Helper()
	state, found, err := database.ClassifyAgentTurnRuntime(ctx, identity)
	if err != nil || !found || state.Identity != identity || state.Disposition != want {
		t.Fatalf("ClassifyAgentTurnRuntime() = (%#v, %t, %v), want %s", state, found, err, want)
	}
}

func TestStartupRecoveryRejectsGenericRunAgentTurnReclaim(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 103)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, databases[0], ctx, turn, time.Second)
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "startup-runtime", time.Second, 1); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, job.ID, turn.ID)
	if count, err := databases[0].ReclaimExpiredJobs(ctx, 10); err != nil || count != 0 {
		t.Fatalf("ReclaimExpiredJobs() = (%d, %v), want (0, nil)", count, err)
	}
	if job, err := databases[0].ClaimJob(ctx, store.AgentTurnQueue, "generic-worker", time.Second); err != nil || job != nil {
		t.Fatalf("ClaimJob() generic RUN_AGENT_TURN reclaim = (%#v, %v), want nil", job, err)
	}
}

func TestPeriodicExpiredTurnMonitorLeavesLiveOwnerThenRecoversAfterLeaseExpiry(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 104)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	leaseDuration := 250 * time.Millisecond
	job := claimAgentTurnJob(t, databases[0], ctx, turn, leaseDuration)
	if _, err := databases[0].AcquireAgentTurn(ctx, job, turn.ControlRevision, "live-at-startup", leaseDuration, 1); err != nil {
		t.Fatal(err)
	}
	identity := store.AgentTurnRuntimeIdentity{
		AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID,
		AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch,
		RuntimeProfileName: "runtime", RuntimeProfileVersion: "1",
	}
	monitor, err := startup.NewMonitor(databases[0], startup.MonitorOptions{
		PollInterval: 10 * time.Millisecond, MaxRecoveriesPerPoll: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- monitor.Run(monitorCtx) }()
	time.Sleep(50 * time.Millisecond)
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeLive)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, found, err := databases[0].ClassifyAgentTurnRuntime(ctx, identity)
		if err != nil {
			t.Fatal(err)
		}
		if found && state.Disposition == store.AgentTurnRuntimeRecovery {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertRuntimeDisposition(t, databases[0], ctx, identity, store.AgentTurnRuntimeRecovery)
	stopMonitor()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Monitor.Run() cancellation error = %v", err)
	}
}

type duplicateStartupRuntimes struct {
	database     *store.Store
	duplicate    startup.DuplicateManagedRuntimeProcess
	absent       bool
	cleanupCalls int
}

func (runtimes *duplicateStartupRuntimes) List(context.Context) (startup.RuntimeInventorySnapshot, error) {
	if runtimes.absent {
		return startup.RuntimeInventorySnapshot{}, nil
	}
	return startup.RuntimeInventorySnapshot{Duplicates: []startup.DuplicateManagedRuntimeProcess{runtimes.duplicate}}, nil
}

func (runtimes *duplicateStartupRuntimes) EnsureAbsent(ctx context.Context, identity store.AgentTurnRuntimeIdentity) error {
	state, found, err := runtimes.database.ClassifyAgentTurnRuntime(ctx, identity)
	if err != nil {
		return err
	}
	if !found || state.Disposition != store.AgentTurnRuntimeRecovery {
		return fmt.Errorf("cleanup observed durable disposition %s, want RECOVERY", state.Disposition)
	}
	runtimes.cleanupCalls++
	runtimes.absent = true
	return nil
}

func (*duplicateStartupRuntimes) EnsureMalformedAbsent(context.Context, startup.MalformedManagedRuntimeProcess) error {
	return nil
}
