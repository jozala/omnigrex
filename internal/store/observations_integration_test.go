//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestDurableObservationsExcludeScheduledAndClaimedWork(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx := context.Background()
	state, err := databases[0].ObserveDurableState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveTurns != 0 || state.UnresolvedMutations != 0 || len(state.Workflows) != 0 {
		t.Fatalf("nonempty state: %+v", state)
	}
	for _, key := range []string{"due", "scheduled", "cancelled"} {
		insertJob(t, pool, ctx, jobSeed{Queue: store.LabelProvisioningQueue, Kind: store.ProvisionManagedLabelsJobKind, MaxAttempts: 3, Payload: json.RawMessage(`{}`), IdempotencyKey: key})
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = clock_timestamp() + CASE WHEN idempotency_key = 'scheduled' THEN interval '1 hour' ELSE interval '-10 minutes' END`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'CANCELLED' WHERE idempotency_key = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	for _, database := range databases { // Fresh process/store reads the same durable state.
		state, err = database.ObserveDurableState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pending := state.Pending["label_provisioning"]
		if pending.Count != 1 || pending.OldestSeconds < 599 || pending.OldestSeconds > 610 {
			t.Fatalf("pending: %+v", pending)
		}
	}
	claim, err := databases[0].ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "observer-test", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	state, err = databases[0].ObserveDurableState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending["label_provisioning"].Count != 0 || state.Pending["label_provisioning"].OldestSeconds != 0 {
		t.Fatal("claimed/future work counted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := databases[0].ObserveDurableState(cancelled); err == nil {
		t.Fatal("failed observation reported as healthy")
	}
}

func TestDurableObservationsTrackExecutionAndUnknownMutations(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := context.Background()
	fixture := seedAgentSession(t, pool, 901)
	turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	state, err := database.ObserveDurableState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending["execution"].Count != 1 || state.ActiveTurns != 0 {
		t.Fatalf("queued: %+v", state)
	}
	for _, payload := range []string{`{"control_revision":999}`, `{"unrecognized_field":true}`} {
		if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = payload || $2::jsonb WHERE agent_turn_id = $1 AND kind = 'RUN_AGENT_TURN'`, turn.ID, payload); err != nil {
			t.Fatal(err)
		}
		state, err = database.ObserveDurableState(ctx)
		if err != nil || state.Pending["execution"].Count != 0 {
			t.Fatalf("invalid payload counted: %+v, %v", state, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = (payload - 'unrecognized_field') || jsonb_build_object('control_revision', $2::bigint) WHERE agent_turn_id = $1 AND kind = 'RUN_AGENT_TURN'`, turn.ID, turn.ControlRevision); err != nil {
			t.Fatal(err)
		}
	}
	// An obsolete preparation has a syntactically valid queue status but no current authority.
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, queue, kind, payload, status, max_attempts, workflow_id, workflow_attempt_id)
SELECT gen_random_uuid(), queue, kind, payload, 'AVAILABLE', 3, workflow_id, workflow_attempt_id
FROM jobs WHERE kind = 'PREPARE_AGENT_TURN' LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.Pending["preparation"].Count != 0 {
		t.Fatalf("obsolete preparation included: %+v, %v", state, err)
	}
	lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "observation", time.Minute, 1)
	if err != nil || !acquired || lease.ID != turn.ID {
		t.Fatalf("acquire: %v", err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.Pending["execution"].Count != 0 || state.ActiveTurns != 1 {
		t.Fatalf("acquired: %+v, %v", state, err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "observed-mutation", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"test"}`), ExternalService: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.UnresolvedMutations != 0 {
		t.Fatalf("ordinary in-flight mutation is recovery backlog: %+v, %v", state, err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET started_at = clock_timestamp() - interval '5 minutes', updated_at = clock_timestamp() WHERE id = $1`, mutation.ID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.UnresolvedMutations != 1 || state.OldestUnresolvedSeconds < 299 {
		t.Fatalf("unknown: %+v, %v", state, err)
	}
	if err := database.BeginMutationReconciliation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.OldestUnresolvedSeconds < 299 {
		t.Fatalf("retry reset unresolved age: %+v, %v", state, err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.UnresolvedMutations != 0 || state.OldestUnresolvedSeconds != 0 {
		t.Fatalf("resolved: %+v, %v", state, err)
	}
}

func TestDurableObservationsWaitForCleanupBeforeMutationRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := context.Background()
	fixture := seedAgentSession(t, pool, 903)
	if _, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec()); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := database.ClaimAndAcquireAgentTurn(ctx, "observer", time.Minute, 1)
	if err != nil || !acquired {
		t.Fatalf("acquire: %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "recovery-observation", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"test"}`), ExternalService: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}
	state, err := database.ObserveDurableState(ctx)
	if err != nil || state.Pending["runtime_cleanup"].Count != 1 || state.Pending["mutation_recovery"].Count != 0 {
		t.Fatalf("before cleanup: %+v, %v", state, err)
	}
	stop, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "observer-stop", time.Minute)
	if err != nil || stop == nil {
		t.Fatalf("claim stop: %v", err)
	}
	if _, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, *stop); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.Pending["runtime_cleanup"].Count != 0 || state.Pending["mutation_recovery"].Count != 1 {
		t.Fatalf("after cleanup: %+v, %v", state, err)
	}
}

func TestDurableObservationsExposeScheduledCorroborationAsActiveNotPending(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := context.Background()
	_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 902, workflow.RoleDeveloper, "review-head")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "observation-checkpoint", ToolName: "request_review", Request: json.RawMessage(`{"operation_id":"observation-checkpoint","summary":"ready"}`),
		ExternalService: "omnigrex", ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"outcome":"REVIEW_REQUESTED"}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "end_turn", ""); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID, json.RawMessage(`{"stop_reason":"end_turn"}`), "", "transient_transport")
	if err != nil {
		t.Fatal(err)
	}
	state, err := database.ObserveDurableState(ctx)
	if err != nil || state.ActiveTurns != 1 || state.Pending["corroboration"].Count != 1 || state.UnresolvedMutations != 0 {
		t.Fatalf("corroboration: %+v, %v", state, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, checkpoint.VerificationJobID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.ActiveTurns != 1 || state.Pending["corroboration"].Count != 0 {
		t.Fatalf("scheduled corroboration: %+v, %v", state, err)
	}
}

func TestDurableObservationQueryPlanWithRetainedHistory(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx := context.Background()
	fixture := seedAgentSession(t, pool, 904)
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
SELECT gen_random_uuid(), 1, 'owner', 'repo', item, item, 'CLOSED' FROM generate_series(1, 20000) item`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, queue, kind, status, max_attempts, completed_at)
SELECT gen_random_uuid(), 'label_provisioning', 'PROVISION_MANAGED_LABELS', 'SUCCEEDED', 3, clock_timestamp() FROM generate_series(1, 20000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tool_invocations (id, agent_turn_id, execution_epoch, operation_lineage_id, invocation_number, tool_name, kind, state, finished_at)
SELECT gen_random_uuid(), turn.id, turn.execution_epoch, turn.operation_lineage_id, item, 'comment_on_issue', 'MUTATION', 'SUCCEEDED', clock_timestamp()
FROM generate_series(1, 20000) item CROSS JOIN agent_turns turn WHERE turn.id = $1`, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH deliveries AS (
INSERT INTO webhook_deliveries (delivery_id, event_name, action, repository_id, issue_id, issue_number, payload, status, processed_at)
SELECT gen_random_uuid(), 'issues', 'reopened', 1, item, item, '{}'::bytea, 'PROCESSED', clock_timestamp() FROM generate_series(1, 20000) item RETURNING delivery_id)
INSERT INTO normalized_events (delivery_id, payload, status, disposition, reason, applied_revision, processed_at)
SELECT delivery_id, '{}'::jsonb, 'COMPLETED', 'UNRELATED', 'observation history', 1, clock_timestamp() FROM deliveries`); err != nil {
		t.Fatal(err)
	}
	// Exercise both blocking anti-joins against retained history, not just empty
	// pending branches: one reopen blocks its later webhook, and one historical
	// event blocks its later same-Work-Item event.
	if _, err := pool.Exec(ctx, `WITH deliveries AS (
INSERT INTO webhook_deliveries (delivery_id, event_name, action, repository_id, issue_id, issue_number, payload, status, received_at, processed_at)
SELECT gen_random_uuid(), event_name, action, 1, issue, issue, '{}'::bytea, status,
       clock_timestamp() - age, CASE WHEN status = 'PROCESSED' THEN clock_timestamp() END
FROM (VALUES ('issues', 'reopened', 30001, 'PENDING', interval '1 minute'),
             ('push', '', 30001, 'PENDING', interval '0 seconds'),
             ('push', '', 30002, 'PENDING', interval '0 seconds'),
             ('issues', 'labeled', 30003, 'PROCESSED', interval '1 minute'),
             ('issues', 'labeled', 30003, 'PROCESSED', interval '0 seconds'))
     AS fixture(event_name, action, issue, status, age)
RETURNING delivery_id, status, received_at)
INSERT INTO normalized_events (delivery_id, payload, status, created_at)
SELECT delivery_id, '{}'::jsonb, 'PENDING', received_at FROM deliveries WHERE status = 'PROCESSED'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE workflows; ANALYZE jobs; ANALYZE tool_invocations; ANALYZE webhook_deliveries; ANALYZE normalized_events`); err != nil {
		t.Fatal(err)
	}
	state, err := databases[0].ObserveDurableState(ctx)
	if err != nil || state.Workflows["CLOSED"] != 20000 {
		t.Fatalf("retained history: %+v, %v", state, err)
	}
	if state.Pending["webhooks"].Count != 2 || state.Pending["historical_events"].Count != 1 {
		t.Fatalf("blocked/eligible event counts: %+v", state.Pending)
	}
	query, err := os.ReadFile("observations.sql")
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var plan json.RawMessage
	if err := pool.QueryRow(bounded, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+string(query), json.RawMessage(`[]`)).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("aggregate plan with 20,000 retained rows in each history table: %s", plan)
}

func TestDurableObservationsPreparationRevisionAndWebhookDelay(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := context.Background()
	application := triggerPreparationWorkflow(t, database, ctx, "99000000-0000-4000-8000-000000000001", "99000000-0000-4000-8000-000000000002")
	state, err := database.ObserveDurableState(ctx)
	if err != nil || state.Pending["preparation"].Count != 1 {
		t.Fatalf("eligible preparation: %+v, %v", state, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET state_revision = state_revision + 1 WHERE id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.Pending["preparation"].Count != 0 {
		t.Fatalf("obsolete revision: %+v, %v", state, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_deliveries (delivery_id, event_name, payload, status, received_at, retry_at)
VALUES (gen_random_uuid(), 'push', '{}'::bytea, 'PENDING', clock_timestamp() - interval '1 hour', clock_timestamp() + interval '1 hour'),
       (gen_random_uuid(), 'push', '{}'::bytea, 'PENDING', clock_timestamp() - interval '1 hour', clock_timestamp() - interval '10 seconds')`); err != nil {
		t.Fatal(err)
	}
	state, err = database.ObserveDurableState(ctx)
	if err != nil || state.Pending["webhooks"].Count != 1 || state.Pending["webhooks"].OldestSeconds < 9 || state.Pending["webhooks"].OldestSeconds > 20 {
		t.Fatalf("webhook retry age: %+v, %v", state, err)
	}
}
