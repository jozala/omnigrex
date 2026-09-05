//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestSettleAgentTurnDurableBoundary(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("Developer initial Pull Request transitions to Reviewing and is idempotent", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 801, workflow.RoleDeveloper, "")
		observation := successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal)
		observation.Completion.Outcome = json.RawMessage(`{"acp_text":"BLOCKED"}`)
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() error = %v", err)
		}
		if settled.Authority != store.AgentTurnSettlementLive || settled.State != workflow.StateReviewing || settled.Reason != workflow.ReasonChangeProposalReady ||
			settled.Revision != 2 || settled.ChangeProposalID == "" || settled.SuccessorJobID == "" ||
			settled.ReconciliationJobID != "" {
			t.Fatalf("Developer settlement = %#v", settled)
		}
		assertSettledExecution(t, pool, ctx, lease, settled)
		var state, createdBy, successorProvenance string
		var active bool
		if err := pool.QueryRow(ctx, `
SELECT workflow.status, proposal.created_by_turn_id::text, proposal.active,
       successor.agent_turn_settlement_id::text
FROM workflows AS workflow
JOIN change_proposals AS proposal ON proposal.workflow_id = workflow.id AND proposal.active
JOIN jobs AS successor ON successor.id = $2
WHERE workflow.id = $1`, fixture.workflowID, settled.SuccessorJobID).Scan(
			&state, &createdBy, &active, &successorProvenance); err != nil {
			t.Fatal(err)
		}
		if state != string(workflow.StateReviewing) || createdBy != lease.ID || !active || successorProvenance != settled.ID {
			t.Errorf("initial Pull Request durability = state %s, creator %s, active %t, provenance %s", state, createdBy, active, successorProvenance)
		}

		replayed, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil || replayed != settled {
			t.Fatalf("idempotent SettleAgentTurn() = (%#v, %v), want %#v", replayed, err, settled)
		}
		conflict := observation
		conflict.Diagnostic = "different observation"
		if _, err := database.SettleAgentTurn(ctx, lease, conflict); !errors.Is(err, store.ErrAgentTurnSettlementConflict) {
			t.Errorf("conflicting settlement replay error = %v", err)
		}
		var settlements, successorJobs int
		if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $1),
       (SELECT count(*) FROM jobs WHERE agent_turn_settlement_id = $2 AND kind = 'PREPARE_AGENT_TURN')`,
			lease.ID, settled.ID).Scan(&settlements, &successorJobs); err != nil {
			t.Fatal(err)
		}
		if settlements != 1 || successorJobs != 1 {
			t.Errorf("idempotent durable rows = %d settlements, %d successors", settlements, successorJobs)
		}
	})

	t.Run("Reviewer approval stores ready head and settlement review provenance", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 802, workflow.RoleReviewer, "review-head")
		review := &workflow.ReviewIdentity{ID: 80201, NodeID: "PRR_802", ChangeProposalID: proposal.PullRequestID, ActorID: 80202, HeadSHA: proposal.HeadSHA}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() approval error = %v", err)
		}
		if settled.State != workflow.StatePRReady || settled.Reason != workflow.ReasonApproved || settled.SuccessorJobID != "" {
			t.Fatalf("approval settlement = %#v", settled)
		}
		var readyFor, provenance string
		var accepted bool
		if err := pool.QueryRow(ctx, `
SELECT proposal.ready_for_sha, review.accepted, review.agent_turn_settlement_id::text
FROM change_proposals AS proposal
JOIN change_proposal_reviews AS review ON review.change_proposal_id = proposal.id
WHERE proposal.workflow_id = $1 AND review.review_id = $2`, fixture.workflowID, review.ID).Scan(
			&readyFor, &accepted, &provenance); err != nil {
			t.Fatal(err)
		}
		if readyFor != proposal.HeadSHA || !accepted || provenance != settled.ID {
			t.Errorf("approval durability = ready %q, accepted %t, provenance %s", readyFor, accepted, provenance)
		}
	})

	t.Run("requested changes consumes budget and creates Developer successor", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 803, workflow.RoleReviewer, "review-head")
		review := &workflow.ReviewIdentity{ID: 80301, NodeID: "PRR_803", ChangeProposalID: proposal.PullRequestID, ActorID: 80302, HeadSHA: proposal.HeadSHA}
		observation := successfulSettlementObservation(workflow.TurnOutcomeChangesRequested, proposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() requested changes error = %v", err)
		}
		if settled.State != workflow.StateDeveloping || settled.Reason != workflow.ReasonChangesRequested || settled.SuccessorJobID == "" {
			t.Fatalf("requested-changes settlement = %#v", settled)
		}
		var used int
		var role, purpose string
		if err := pool.QueryRow(ctx, `
SELECT attempt.review_cycles_completed, successor.payload->>'role', successor.payload->>'purpose'
FROM workflow_attempts AS attempt
JOIN jobs AS successor ON successor.id = $2
WHERE attempt.id = $1`, fixture.attemptID, settled.SuccessorJobID).Scan(&used, &role, &purpose); err != nil {
			t.Fatal(err)
		}
		if used != 1 || role != string(workflow.RoleDeveloper) || purpose != string(workflow.TurnPurposeRequestedChanges) {
			t.Errorf("requested-changes durability = budget %d, successor %s/%s", used, role, purpose)
		}
	})

	t.Run("infrastructure failure consumes retry budget and retries exact Role", func(t *testing.T) {
		fixture, lease, _ := prepareSettlementTurn(t, database, pool, ctx, 804, workflow.RoleDeveloper, "")
		observation := failedSettlementObservation("runtime transport failed")
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() infrastructure retry error = %v", err)
		}
		if settled.Reason != workflow.ReasonInfrastructureRetry || settled.State != workflow.StateDeveloping || settled.SuccessorJobID == "" {
			t.Fatalf("infrastructure settlement = %#v", settled)
		}
		var used int
		var role, purpose, retryOf string
		if err := pool.QueryRow(ctx, `
SELECT attempt.infrastructure_failures, successor.payload->>'role',
       successor.payload->>'purpose', successor.payload->>'retry_of_turn_id'
FROM workflow_attempts AS attempt JOIN jobs AS successor ON successor.id = $2
WHERE attempt.id = $1`, fixture.attemptID, settled.SuccessorJobID).Scan(&used, &role, &purpose, &retryOf); err != nil {
			t.Fatal(err)
		}
		if used != 1 || role != string(workflow.RoleDeveloper) || purpose != string(workflow.TurnPurposeRetry) || retryOf != lease.ID {
			t.Errorf("infrastructure retry = budget %d, %s/%s retry %s", used, role, purpose, retryOf)
		}
	})

	t.Run("blocked outcome creates Human Handoff with sanitized diagnostic", func(t *testing.T) {
		fixture, lease, _ := prepareSettlementTurn(t, database, pool, ctx, 805, workflow.RoleDeveloper, "")
		observation := successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil)
		observation.Diagnostic = " blocked\n\tdiagnostic\x00 "
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() blocked error = %v", err)
		}
		if settled.State != workflow.StateNeedsHuman || settled.Reason != workflow.ReasonAgentBlocked || settled.SuccessorJobID != "" {
			t.Fatalf("blocked settlement = %#v", settled)
		}
		var reason, diagnostic, provenance string
		if err := pool.QueryRow(ctx, `
SELECT workflow.human_handoff_reason, handoff.payload->>'diagnostic',
       handoff.agent_turn_settlement_id::text
FROM workflows AS workflow
JOIN jobs AS handoff ON handoff.workflow_id = workflow.id AND handoff.kind = 'PUBLISH_HUMAN_HANDOFF'
WHERE workflow.id = $1`, fixture.workflowID).Scan(&reason, &diagnostic, &provenance); err != nil {
			t.Fatal(err)
		}
		if reason != string(workflow.ReasonAgentBlocked) || diagnostic != "blocked diagnostic" || provenance != settled.ID {
			t.Errorf("Human Handoff = reason %s, diagnostic %q, provenance %s", reason, diagnostic, provenance)
		}
	})

	t.Run("stale review head does not consume budget", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 806, workflow.RoleReviewer, "old-head")
		current := *proposal
		current.HeadSHA = "new-head"
		review := &workflow.ReviewIdentity{ID: 80601, NodeID: "PRR_806", ChangeProposalID: proposal.PullRequestID, ActorID: 80602, HeadSHA: "old-head"}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, &current)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() stale review error = %v", err)
		}
		if settled.Reason != workflow.ReasonReviewHeadReplaced || settled.State != workflow.StateReviewing || settled.SuccessorJobID == "" {
			t.Fatalf("stale review settlement = %#v", settled)
		}
		var used int
		var head, expected, purpose string
		if err := pool.QueryRow(ctx, `
SELECT attempt.review_cycles_completed, proposal.head_sha,
       successor.payload->>'expected_head_sha', successor.payload->>'purpose'
FROM workflow_attempts AS attempt
JOIN change_proposals AS proposal ON proposal.workflow_id = attempt.workflow_id AND proposal.active
JOIN jobs AS successor ON successor.id = $2
WHERE attempt.id = $1`, fixture.attemptID, settled.SuccessorJobID).Scan(&used, &head, &expected, &purpose); err != nil {
			t.Fatal(err)
		}
		if used != 0 || head != "new-head" || expected != "new-head" || purpose != string(workflow.TurnPurposeSynchronization) {
			t.Errorf("stale review = budget %d, heads %s/%s, purpose %s", used, head, expected, purpose)
		}
	})

	t.Run("pending events create reconciliation instead of direct successor", func(t *testing.T) {
		_, lease, _ := prepareSettlementTurn(t, database, pool, ctx, 807, workflow.RoleDeveloper, "")
		deferredID := "78070000-0000-4000-8000-000000000001"
		insertDeferredSettlementEvent(t, pool, ctx, deferredID, lease.JobLease.WorkflowID, lease.ID, "observed-head")
		settled, err := database.SettleAgentTurn(ctx, lease, failedSettlementObservation("runtime failed"))
		if err != nil {
			t.Fatalf("SettleAgentTurn() with pending event error = %v", err)
		}
		if settled.PendingEventCount != 1 || settled.LatestObservedHeadSHA != "" ||
			settled.ReconciliationJobID == "" || settled.SuccessorJobID != "" {
			t.Fatalf("pending-event settlement = %#v", settled)
		}
		var reconciliation, successors, links int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'RECONCILE_PENDING_EVENTS'),
       count(*) FILTER (WHERE kind = 'PREPARE_AGENT_TURN'),
       (SELECT count(*) FROM job_normalized_events WHERE normalized_event_id = $2)
FROM jobs WHERE agent_turn_settlement_id = $1`, settled.ID, deferredID).Scan(
			&reconciliation, &successors, &links); err != nil {
			t.Fatal(err)
		}
		if reconciliation != 1 || successors != 0 || links != 1 {
			t.Errorf("pending actions = reconciliation %d, successors %d, links %d", reconciliation, successors, links)
		}
		reconciliationLease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "settlement-reconciler", 20*time.Second)
		if err != nil || reconciliationLease == nil || reconciliationLease.ID != settled.ReconciliationJobID {
			t.Fatalf("claim settlement reconciliation = (%#v, %v)", reconciliationLease, err)
		}
		acknowledgement, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliationLease,
			func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
				return store.WorkflowLocator{RepositoryID: 807, IssueID: 807, IssueNumber: 807}, func(snapshot workflow.Snapshot) workflow.Decision {
					return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionUnrelated, Reason: workflow.ReasonCorroborationWithoutActiveTurn}
				}, nil
			})
		if err != nil || acknowledgement.SuccessorJobID == "" {
			t.Fatalf("acknowledge settlement reconciliation = (%#v, %v)", acknowledgement, err)
		}
		var successorProvenance string
		if err := pool.QueryRow(ctx, `SELECT agent_turn_settlement_id::text FROM jobs WHERE id = $1`, acknowledgement.SuccessorJobID).Scan(&successorProvenance); err != nil {
			t.Fatal(err)
		}
		if successorProvenance != settled.ID {
			t.Errorf("reconciled successor provenance = %s, want %s", successorProvenance, settled.ID)
		}
	})

	t.Run("deferred synchronization wins durable order over review and open events", func(t *testing.T) {
		tests := []struct {
			name                      string
			number                    int
			events                    []deferredSettlementEvent
			observedHead              string
			latestObservedHead        string
			finalHead                 string
			appliedSynchronizations   int
			staleSynchronizations     int
			duplicateSynchronizations int
			expectedFactoryOrder      []string
		}{
			{
				name: "synchronization before review", number: 821,
				latestObservedHead: "head-2", finalHead: "head-2", appliedSynchronizations: 1,
				events: []deferredSettlementEvent{
					{deliveryID: "78210000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
					{deliveryID: "78210000-0000-4000-8000-000000000002", eventName: "pull_request_review", action: "submitted", headSHA: "head-1"},
				},
			},
			{
				name: "review before synchronization", number: 822,
				latestObservedHead: "head-2", finalHead: "head-2", appliedSynchronizations: 1,
				events: []deferredSettlementEvent{
					{deliveryID: "78220000-0000-4000-8000-000000000001", eventName: "pull_request_review", action: "submitted", headSHA: "head-1"},
					{deliveryID: "78220000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
				},
			},
			{
				name: "latest of multiple synchronizations", number: 823,
				latestObservedHead: "head-3", finalHead: "head-3", appliedSynchronizations: 2,
				events: []deferredSettlementEvent{
					{deliveryID: "78230000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
					{deliveryID: "78230000-0000-4000-8000-000000000002", eventName: "pull_request_review", action: "submitted", headSHA: "review-noise"},
					{deliveryID: "78230000-0000-4000-8000-000000000003", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
					{deliveryID: "78230000-0000-4000-8000-000000000004", eventName: "pull_request", action: "opened", headSHA: "open-noise"},
				},
			},
			{
				name: "reverse causal chain across review and open events", number: 825,
				latestObservedHead: "head-2", finalHead: "head-4", appliedSynchronizations: 3,
				expectedFactoryOrder: []string{
					"78250000-0000-4000-8000-000000000005",
					"78250000-0000-4000-8000-000000000003",
					"78250000-0000-4000-8000-000000000001",
					"78250000-0000-4000-8000-000000000002",
					"78250000-0000-4000-8000-000000000004",
				},
				events: []deferredSettlementEvent{
					{deliveryID: "78250000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-3", headSHA: "head-4"},
					{deliveryID: "78250000-0000-4000-8000-000000000002", eventName: "pull_request_review", action: "submitted", headSHA: "head-1"},
					{deliveryID: "78250000-0000-4000-8000-000000000003", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
					{deliveryID: "78250000-0000-4000-8000-000000000004", eventName: "pull_request", action: "opened", headSHA: "open-noise"},
					{deliveryID: "78250000-0000-4000-8000-000000000005", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
				},
			},
			{
				name: "fresh settlement observation already at reverse chain tip", number: 830,
				observedHead: "head-3", latestObservedHead: "head-2", finalHead: "head-3",
				staleSynchronizations: 1, duplicateSynchronizations: 1,
				expectedFactoryOrder: []string{
					"78300000-0000-4000-8000-000000000002",
					"78300000-0000-4000-8000-000000000001",
				},
				events: []deferredSettlementEvent{
					{deliveryID: "78300000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
					{deliveryID: "78300000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
				},
			},
			{
				name: "fresh settlement observation at chain interior", number: 831,
				observedHead: "head-2", latestObservedHead: "head-2", finalHead: "head-3",
				appliedSynchronizations: 1, duplicateSynchronizations: 1,
				expectedFactoryOrder: []string{
					"78310000-0000-4000-8000-000000000002",
					"78310000-0000-4000-8000-000000000001",
				},
				events: []deferredSettlementEvent{
					{deliveryID: "78310000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
					{deliveryID: "78310000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
				},
			},
			{
				name: "same edge redeliveries at chain tip", number: 832,
				observedHead: "head-2", latestObservedHead: "head-2", finalHead: "head-2",
				duplicateSynchronizations: 2,
				expectedFactoryOrder: []string{
					"78320000-0000-4000-8000-000000000001",
					"78320000-0000-4000-8000-000000000002",
				},
				events: []deferredSettlementEvent{
					{deliveryID: "78320000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
					{deliveryID: "78320000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, test.number, workflow.RoleReviewer, "head-1")
				baseTime := time.Date(2026, time.September, 4, 10, 5, 0, 0, time.UTC)
				for index, event := range test.events {
					event.createdAt = baseTime.Add(time.Duration(index) * time.Millisecond)
					insertDeferredSettlementNormalizedEvent(t, pool, ctx, lease.JobLease.WorkflowID, lease.ID, event)
				}
				review := &workflow.ReviewIdentity{
					ID: int64(test.number*100 + 1), NodeID: fmt.Sprintf("PRR_%d", test.number),
					ChangeProposalID: proposal.PullRequestID, ActorID: int64(test.number*100 + 2), HeadSHA: "head-1",
				}
				observedProposal := proposal
				if test.observedHead != "" {
					fresh := *proposal
					fresh.HeadSHA = test.observedHead
					observedProposal = &fresh
				}
				observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, observedProposal)
				observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID

				settled, err := database.SettleAgentTurn(ctx, lease, observation)
				if err != nil {
					t.Fatalf("SettleAgentTurn() synchronization race error = %v", err)
				}
				if settled.Reason != workflow.ReasonReviewHeadReplaced || settled.State != workflow.StateReviewing ||
					settled.PendingEventCount != uint32(len(test.events)) || settled.LatestObservedHeadSHA != test.latestObservedHead ||
					settled.ReconciliationJobID == "" || settled.SuccessorJobID != "" {
					t.Fatalf("synchronization-race settlement = %#v", settled)
				}
				var used int
				var accepted bool
				var latest, fallbackRole, fallbackPurpose, fallbackHead string
				if err := pool.QueryRow(ctx, `
SELECT attempt.review_cycles_completed, review.accepted,
       reconciliation.payload->>'latest_observed_head_sha',
       reconciliation.payload->>'fallback_role',
       reconciliation.payload->>'fallback_purpose',
       reconciliation.payload->>'fallback_expected_head_sha'
FROM workflow_attempts AS attempt
JOIN change_proposals AS proposal ON proposal.workflow_id = attempt.workflow_id AND proposal.active
JOIN change_proposal_reviews AS review ON review.change_proposal_id = proposal.id AND review.review_id = $2
JOIN jobs AS reconciliation ON reconciliation.id = $3
WHERE attempt.id = $1`, fixture.attemptID, review.ID, settled.ReconciliationJobID).Scan(
					&used, &accepted, &latest, &fallbackRole, &fallbackPurpose, &fallbackHead); err != nil {
					t.Fatal(err)
				}
				expectedFallbackHead := "head-1"
				if test.observedHead != "" {
					expectedFallbackHead = test.observedHead
				}
				if used != 0 || accepted || latest != test.latestObservedHead || fallbackRole != string(workflow.RoleReviewer) ||
					fallbackPurpose != string(workflow.TurnPurposeSynchronization) || fallbackHead != expectedFallbackHead {
					t.Errorf("synchronization-race durability = budget %d, accepted %t, reconciliation %s/%s/%s at %s",
						used, accepted, fallbackRole, fallbackPurpose, fallbackHead, latest)
				}

				reconciliationLease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "race-reconciler", 20*time.Second)
				if err != nil || reconciliationLease == nil || reconciliationLease.ID != settled.ReconciliationJobID {
					t.Fatalf("claim synchronization-race reconciliation = (%#v, %v)", reconciliationLease, err)
				}
				var factoryOrder []string
				factory := settlementPendingTransitionFactory
				if len(test.expectedFactoryOrder) > 0 {
					factory = func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
						factoryOrder = append(factoryOrder, record.DeliveryID)
						return settlementPendingTransitionFactory(record)
					}
				}
				acknowledgement, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliationLease, factory)
				if err != nil {
					t.Fatalf("acknowledge synchronization-race reconciliation = %v", err)
				}
				if acknowledgement.SuccessorJobID == "" || acknowledgement.Successor.Role != workflow.RoleReviewer ||
					acknowledgement.Successor.Purpose != workflow.TurnPurposeSynchronization || acknowledgement.Successor.ExpectedHeadSHA != test.finalHead {
					t.Errorf("synchronization-race successor = %#v", acknowledgement)
				}
				eventIDs := make([]string, len(test.events))
				for index := range test.events {
					eventIDs[index] = test.events[index].deliveryID
				}
				var head, readyForSHA, successorHead string
				var completedEvents, appliedSynchronizations, staleSynchronizations, duplicateSynchronizations int
				var unrelatedCorroborations, successors, finalLabelJobs, handoffs int
				if err := pool.QueryRow(ctx, `
SELECT proposal.head_sha, COALESCE(proposal.ready_for_sha, ''),
		count(*) FILTER (WHERE event.delivery_id = ANY($2) AND event.status = 'COMPLETED'),
        count(*) FILTER (WHERE delivery.event_name = 'pull_request' AND delivery.action = 'synchronize'
            AND event.disposition = 'APPLIED'),
		count(*) FILTER (WHERE delivery.event_name = 'pull_request' AND delivery.action = 'synchronize'
			AND event.disposition = 'STALE'),
		count(*) FILTER (WHERE delivery.event_name = 'pull_request' AND delivery.action = 'synchronize'
			AND event.disposition = 'DUPLICATE'),
        count(*) FILTER (WHERE delivery.event_name IN ('pull_request', 'pull_request_review')
            AND delivery.action <> 'synchronize' AND event.disposition = 'UNRELATED'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'
           AND status IN ('AVAILABLE', 'LEASED')),
       (SELECT min(payload->>'expected_head_sha') FROM jobs WHERE workflow_id = $1
           AND kind = 'PREPARE_AGENT_TURN' AND status IN ('AVAILABLE', 'LEASED')),
       (SELECT count(*) FROM jobs AS labels
        JOIN workflows AS current ON current.id = labels.workflow_id
         WHERE labels.workflow_id = $1 AND labels.kind = 'RECONCILE_GITHUB_LABELS'
           AND (labels.payload->>'revision')::bigint = current.state_revision
           AND labels.payload->>'state' = 'REVIEWING'
		   AND COALESCE(labels.payload->>'ready_for_sha', '') = ''),
		(SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF')
FROM change_proposals AS proposal
JOIN normalized_events AS event ON event.workflow_id = proposal.workflow_id
JOIN webhook_deliveries AS delivery USING (delivery_id)
WHERE proposal.workflow_id = $1 AND proposal.active
GROUP BY proposal.head_sha, proposal.ready_for_sha`, fixture.workflowID, eventIDs).Scan(
					&head, &readyForSHA, &completedEvents, &appliedSynchronizations, &staleSynchronizations,
					&duplicateSynchronizations, &unrelatedCorroborations, &successors, &successorHead,
					&finalLabelJobs, &handoffs); err != nil {
					t.Fatal(err)
				}
				if head != test.finalHead || readyForSHA != "" || completedEvents != len(test.events) ||
					appliedSynchronizations != test.appliedSynchronizations || staleSynchronizations != test.staleSynchronizations ||
					duplicateSynchronizations != test.duplicateSynchronizations || successors != 1 ||
					successorHead != test.finalHead || finalLabelJobs != 1 || handoffs != 0 {
					t.Errorf("causal replay = head %q ready %q, completed %d, syncs applied/stale/duplicate %d/%d/%d, unrelated corroborations %d, successors %d at %q, final labels %d, handoffs %d",
						head, readyForSHA, completedEvents, appliedSynchronizations, staleSynchronizations,
						duplicateSynchronizations, unrelatedCorroborations, successors, successorHead, finalLabelJobs, handoffs)
				}
				if len(test.expectedFactoryOrder) > 0 && fmt.Sprint(factoryOrder) != fmt.Sprint(test.expectedFactoryOrder) {
					t.Errorf("causal factory order = %v, want %v", factoryOrder, test.expectedFactoryOrder)
				}
				if test.number == 825 {
					rows, err := pool.Query(ctx, `
SELECT delivery_id::text FROM normalized_events
WHERE delivery_id = ANY($1) AND disposition = 'APPLIED'
ORDER BY applied_revision, delivery_id`, []string{
						"78250000-0000-4000-8000-000000000001",
						"78250000-0000-4000-8000-000000000003",
						"78250000-0000-4000-8000-000000000005",
					})
					if err != nil {
						t.Fatal(err)
					}
					var replayOrder []string
					for rows.Next() {
						var deliveryID string
						if err := rows.Scan(&deliveryID); err != nil {
							rows.Close()
							t.Fatal(err)
						}
						replayOrder = append(replayOrder, deliveryID)
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					rows.Close()
					want := []string{
						"78250000-0000-4000-8000-000000000005",
						"78250000-0000-4000-8000-000000000003",
						"78250000-0000-4000-8000-000000000001",
					}
					if fmt.Sprint(replayOrder) != fmt.Sprint(want) {
						t.Errorf("causal synchronization replay order = %v, want %v", replayOrder, want)
					}
				}
			})
		}
	})

	t.Run("same-head review webhook does not make settlement stale", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 824, workflow.RoleReviewer, "head-1")
		insertDeferredSettlementNormalizedEvent(t, pool, ctx, lease.JobLease.WorkflowID, lease.ID, deferredSettlementEvent{
			deliveryID: "78240000-0000-4000-8000-000000000001", eventName: "pull_request_review",
			action: "submitted", headSHA: "head-1", createdAt: time.Date(2026, time.September, 4, 10, 6, 0, 0, time.UTC),
		})
		review := &workflow.ReviewIdentity{ID: 82401, NodeID: "PRR_824", ChangeProposalID: proposal.PullRequestID, ActorID: 82402, HeadSHA: "head-1"}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID

		settled, err := database.SettleAgentTurn(ctx, lease, observation)
		if err != nil {
			t.Fatalf("SettleAgentTurn() same-head review webhook error = %v", err)
		}
		if settled.Reason != workflow.ReasonApproved || settled.State != workflow.StatePRReady ||
			settled.LatestObservedHeadSHA != "" || settled.ReconciliationJobID == "" {
			t.Fatalf("same-head review webhook settlement = %#v", settled)
		}
		var used int
		var accepted bool
		if err := pool.QueryRow(ctx, `
SELECT attempt.review_cycles_completed, review.accepted
FROM workflow_attempts AS attempt
JOIN change_proposals AS proposal ON proposal.workflow_id = attempt.workflow_id AND proposal.active
JOIN change_proposal_reviews AS review ON review.change_proposal_id = proposal.id AND review.review_id = $2
WHERE attempt.id = $1`, fixture.attemptID, review.ID).Scan(&used, &accepted); err != nil {
			t.Fatal(err)
		}
		if used != 1 || !accepted {
			t.Errorf("same-head review webhook durability = budget %d, accepted %t", used, accepted)
		}
		reconciliationLease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "same-head-review-reconciler", 20*time.Second)
		if err != nil || reconciliationLease == nil || reconciliationLease.ID != settled.ReconciliationJobID {
			t.Fatalf("claim same-head review reconciliation = (%#v, %v)", reconciliationLease, err)
		}
		if _, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliationLease, settlementPendingTransitionFactory); err != nil {
			t.Fatalf("acknowledge same-head review reconciliation = %v", err)
		}
	})

	t.Run("same-head synchronization redelivery terminalizes as duplicate", func(t *testing.T) {
		fixture, turnLease, proposal := prepareSettlementTurn(t, database, pool, ctx, 826, workflow.RoleReviewer, "head-1")
		deliveryID := "78260000-0000-4000-8000-000000000001"
		insertDeferredSettlementNormalizedEvent(t, pool, ctx, turnLease.JobLease.WorkflowID, turnLease.ID, deferredSettlementEvent{
			deliveryID: deliveryID, eventName: "pull_request", action: "synchronize",
			beforeSHA: "head-1", headSHA: "head-2", createdAt: time.Date(2026, time.September, 4, 10, 7, 0, 0, time.UTC),
		})
		observedProposal := *proposal
		observedProposal.HeadSHA = "head-2"
		review := &workflow.ReviewIdentity{ID: 82601, NodeID: "PRR_826", ChangeProposalID: proposal.PullRequestID, ActorID: 82602, HeadSHA: "head-1"}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, &observedProposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, turnLease, observation)
		if err != nil || settled.ReconciliationJobID == "" {
			t.Fatalf("SettleAgentTurn() same-head redelivery = (%#v, %v)", settled, err)
		}
		reconciliationLease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "duplicate-reconciler", 20*time.Second)
		if err != nil || reconciliationLease == nil || reconciliationLease.ID != settled.ReconciliationJobID {
			t.Fatalf("claim same-head reconciliation = (%#v, %v)", reconciliationLease, err)
		}
		acknowledgement, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliationLease, settlementPendingTransitionFactory)
		if err != nil || acknowledgement.SuccessorJobID == "" || acknowledgement.Successor.ExpectedHeadSHA != "head-2" {
			t.Fatalf("acknowledge same-head redelivery = (%#v, %v)", acknowledgement, err)
		}
		var head, status, disposition, reason string
		if err := pool.QueryRow(ctx, `
SELECT proposal.head_sha, event.status, event.disposition, event.reason
FROM change_proposals AS proposal
JOIN normalized_events AS event ON event.workflow_id = proposal.workflow_id
WHERE proposal.workflow_id = $1 AND proposal.active AND event.delivery_id = $2`, fixture.workflowID, deliveryID).Scan(
			&head, &status, &disposition, &reason); err != nil {
			t.Fatal(err)
		}
		if head != "head-2" || status != string(store.NormalizedEventCompleted) ||
			disposition != string(workflow.DispositionDuplicate) || reason != string(workflow.ReasonSynchronizationDuplicate) {
			t.Errorf("same-head redelivery = head %q event %s/%s/%s", head, status, disposition, reason)
		}
	})

	t.Run("malformed synchronization identity remains a permanent replay failure", func(t *testing.T) {
		_, turnLease, proposal := prepareSettlementTurn(t, database, pool, ctx, 829, workflow.RoleReviewer, "head-1")
		deliveryID := "78290000-0000-4000-8000-000000000001"
		insertDeferredSettlementNormalizedEvent(t, pool, ctx, turnLease.JobLease.WorkflowID, turnLease.ID, deferredSettlementEvent{
			deliveryID: deliveryID, eventName: "pull_request", action: "synchronize",
			headSHA: "head-2", createdAt: time.Date(2026, time.September, 4, 10, 7, 30, 0, time.UTC),
		})
		review := &workflow.ReviewIdentity{ID: 82901, NodeID: "PRR_829", ChangeProposalID: proposal.PullRequestID, ActorID: 82902, HeadSHA: "head-1"}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, turnLease, observation)
		if err != nil || settled.ReconciliationJobID == "" {
			t.Fatalf("SettleAgentTurn() malformed synchronization = (%#v, %v)", settled, err)
		}
		lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "malformed-reconciler", 20*time.Second)
		if err != nil || lease == nil || lease.ID != settled.ReconciliationJobID {
			t.Fatalf("claim malformed reconciliation = (%#v, %v)", lease, err)
		}
		if _, err := database.AcknowledgePendingEventReconciliation(ctx, *lease, settlementPendingTransitionFactory); !errors.Is(err, store.ErrPendingNormalizedEventInvalid) {
			t.Fatalf("malformed synchronization replay error = %v", err)
		}
		failure, err := database.AcknowledgePendingEventReconciliationFailure(ctx, *lease, store.ErrPendingNormalizedEventInvalid, false, 0)
		if err != nil || failure.RetryScheduled || !failure.EscalationScheduled {
			t.Fatalf("acknowledge malformed synchronization failure = (%#v, %v)", failure, err)
		}
		escalation, err := database.ApplyNextWorkflowActionFailureEscalation(ctx, "malformed-escalation", 20*time.Second)
		if err != nil || escalation == nil || !escalation.HandoffApplied || escalation.DeferredEventsCompleted != 1 {
			t.Fatalf("ApplyNextWorkflowActionFailureEscalation() malformed synchronization = (%#v, %v)", escalation, err)
		}
		var eventStatus, jobStatus string
		if err := pool.QueryRow(ctx, `
SELECT event.status, job.status FROM normalized_events AS event
JOIN jobs AS job ON job.id = $2 WHERE event.delivery_id = $1`, deliveryID, settled.ReconciliationJobID).Scan(
			&eventStatus, &jobStatus); err != nil {
			t.Fatal(err)
		}
		if eventStatus != string(store.NormalizedEventCompleted) || jobStatus != string(store.JobFailed) {
			t.Errorf("malformed synchronization durability = event %s, job %s", eventStatus, jobStatus)
		}
	})

	t.Run("ambiguous synchronization topologies retain deferred rows", func(t *testing.T) {
		tests := []struct {
			name   string
			number int
			events []deferredSettlementEvent
		}{
			{
				name: "fork", number: 833,
				events: []deferredSettlementEvent{
					{deliveryID: "78330000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
					{deliveryID: "78330000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-3"},
				},
			},
			{
				name: "ambiguous merge", number: 834,
				events: []deferredSettlementEvent{
					{deliveryID: "78340000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-3"},
					{deliveryID: "78340000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
				},
			},
			{
				name: "cycle", number: 835,
				events: []deferredSettlementEvent{
					{deliveryID: "78350000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
					{deliveryID: "78350000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-1"},
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fixture, turnLease, proposal := prepareSettlementTurn(t, database, pool, ctx, test.number, workflow.RoleReviewer, "head-1")
				baseTime := time.Date(2026, time.September, 4, 10, 7, 45, 0, time.UTC)
				eventIDs := make([]string, len(test.events))
				for index, event := range test.events {
					event.createdAt = baseTime.Add(time.Duration(index) * time.Millisecond)
					eventIDs[index] = event.deliveryID
					insertDeferredSettlementNormalizedEvent(t, pool, ctx, turnLease.JobLease.WorkflowID, turnLease.ID, event)
				}
				review := &workflow.ReviewIdentity{
					ID: int64(test.number*100 + 1), NodeID: fmt.Sprintf("PRR_%d", test.number),
					ChangeProposalID: proposal.PullRequestID, ActorID: int64(test.number*100 + 2), HeadSHA: "head-1",
				}
				observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
				observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
				settled, err := database.SettleAgentTurn(ctx, turnLease, observation)
				if err != nil || settled.ReconciliationJobID == "" {
					t.Fatalf("SettleAgentTurn() ambiguous topology = (%#v, %v)", settled, err)
				}
				lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "ambiguous-reconciler", 20*time.Second)
				if err != nil || lease == nil || lease.ID != settled.ReconciliationJobID {
					t.Fatalf("claim ambiguous reconciliation = (%#v, %v)", lease, err)
				}
				factoryCalled := false
				_, err = database.AcknowledgePendingEventReconciliation(ctx, *lease, func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
					factoryCalled = true
					return settlementPendingTransitionFactory(record)
				})
				if !errors.Is(err, store.ErrPendingEventCausalGap) {
					t.Fatalf("ambiguous reconciliation error = %v, want ErrPendingEventCausalGap", err)
				}
				if factoryCalled {
					t.Error("transition factory called for ambiguous synchronization topology")
				}
				var head, jobStatus string
				var deferred, handoffs int
				if err := pool.QueryRow(ctx, `
SELECT proposal.head_sha, job.status,
       (SELECT count(*) FROM normalized_events WHERE delivery_id = ANY($3) AND status = 'DEFERRED'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF')
FROM change_proposals AS proposal
JOIN jobs AS job ON job.id = $2
WHERE proposal.workflow_id = $1 AND proposal.active`, fixture.workflowID, settled.ReconciliationJobID, eventIDs).Scan(
					&head, &jobStatus, &deferred, &handoffs); err != nil {
					t.Fatal(err)
				}
				if head != "head-1" || jobStatus != string(store.JobLeased) || deferred != len(test.events) || handoffs != 0 {
					t.Errorf("ambiguous topology durability = head %q, job %s, deferred %d, handoffs %d", head, jobStatus, deferred, handoffs)
				}
			})
		}
	})

	t.Run("disconnected synchronization retries then reaches Human Handoff", func(t *testing.T) {
		fixture, turnLease, proposal := prepareSettlementTurn(t, database, pool, ctx, 827, workflow.RoleReviewer, "head-1")
		deliveryID := "78270000-0000-4000-8000-000000000001"
		insertDeferredSettlementNormalizedEvent(t, pool, ctx, turnLease.JobLease.WorkflowID, turnLease.ID, deferredSettlementEvent{
			deliveryID: deliveryID, eventName: "pull_request", action: "synchronize",
			beforeSHA: "fork-head", headSHA: "fork-successor", createdAt: time.Date(2026, time.September, 4, 10, 8, 0, 0, time.UTC),
		})
		review := &workflow.ReviewIdentity{ID: 82701, NodeID: "PRR_827", ChangeProposalID: proposal.PullRequestID, ActorID: 82702, HeadSHA: "head-1"}
		observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
		observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
		settled, err := database.SettleAgentTurn(ctx, turnLease, observation)
		if err != nil || settled.ReconciliationJobID == "" {
			t.Fatalf("SettleAgentTurn() disconnected synchronization = (%#v, %v)", settled, err)
		}

		for attempt := 1; attempt <= 3; attempt++ {
			lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, fmt.Sprintf("gap-reconciler-%d", attempt), 20*time.Second)
			if err != nil || lease == nil || lease.ID != settled.ReconciliationJobID || lease.Attempt != attempt {
				t.Fatalf("claim causal-gap attempt %d = (%#v, %v)", attempt, lease, err)
			}
			if _, err := database.AcknowledgePendingEventReconciliation(ctx, *lease, settlementPendingTransitionFactory); !errors.Is(err, store.ErrPendingEventCausalGap) {
				t.Fatalf("causal-gap attempt %d error = %v", attempt, err)
			}
			var eventStatus, jobStatus string
			if err := pool.QueryRow(ctx, `
SELECT event.status, job.status
FROM normalized_events AS event
JOIN jobs AS job ON job.id = $2
WHERE event.delivery_id = $1`, deliveryID, settled.ReconciliationJobID).Scan(&eventStatus, &jobStatus); err != nil {
				t.Fatal(err)
			}
			if eventStatus != string(store.NormalizedEventDeferred) || jobStatus != string(store.JobLeased) {
				t.Fatalf("causal-gap attempt %d durability = event %s, job %s", attempt, eventStatus, jobStatus)
			}
			failure, err := database.AcknowledgePendingEventReconciliationFailure(ctx, *lease, store.ErrPendingEventCausalGap, true, 0)
			if err != nil || failure.RetryScheduled != (attempt < 3) || failure.EscalationScheduled != (attempt == 3) {
				t.Fatalf("acknowledge causal-gap failure %d = (%#v, %v)", attempt, failure, err)
			}
		}
		escalation, err := database.ApplyNextWorkflowActionFailureEscalation(ctx, "causal-gap-escalation", 20*time.Second)
		if err != nil || escalation == nil || !escalation.HandoffApplied || escalation.DeferredEventsCompleted != 1 {
			t.Fatalf("ApplyNextWorkflowActionFailureEscalation() causal gap = (%#v, %v)", escalation, err)
		}
		var workflowState, eventStatus, disposition, reason, sourceStatus string
		var head string
		if err := pool.QueryRow(ctx, `
SELECT workflow.status, proposal.head_sha, event.status, event.disposition, event.reason, source.status
FROM workflows AS workflow
JOIN change_proposals AS proposal ON proposal.workflow_id = workflow.id AND proposal.active
JOIN normalized_events AS event ON event.workflow_id = workflow.id AND event.delivery_id = $2
JOIN jobs AS source ON source.id = $3
WHERE workflow.id = $1`, fixture.workflowID, deliveryID, settled.ReconciliationJobID).Scan(
			&workflowState, &head, &eventStatus, &disposition, &reason, &sourceStatus); err != nil {
			t.Fatal(err)
		}
		if workflowState != string(workflow.StateNeedsHuman) || head != "head-1" || sourceStatus != string(store.JobFailed) ||
			eventStatus != string(store.NormalizedEventCompleted) || disposition != string(workflow.DispositionReconciliationFailed) ||
			reason != string(workflow.ReasonWorkflowActionExhausted) {
			t.Errorf("causal-gap terminal state = Workflow %s head %q, source %s, event %s/%s/%s",
				workflowState, head, sourceStatus, eventStatus, disposition, reason)
		}
	})

	t.Run("duplicate and illegal reducer dispositions do not finalize", func(t *testing.T) {
		for index, mutate := range []func(*store.AgentTurnSettlementObservation, *workflow.ReviewIdentity){
			func(observation *store.AgentTurnSettlementObservation, review *workflow.ReviewIdentity) {
				existing := *review
				observation.ExistingReview = &existing
			},
			func(observation *store.AgentTurnSettlementObservation, review *workflow.ReviewIdentity) {
				existing := *review
				existing.NodeID = "conflicting-review-node"
				observation.ExistingReview = &existing
			},
		} {
			number := 811 + index
			fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, number, workflow.RoleReviewer, "review-head")
			review := &workflow.ReviewIdentity{
				ID: int64(number*100 + 1), NodeID: fmt.Sprintf("PRR_%d", number),
				ChangeProposalID: proposal.PullRequestID, ActorID: int64(number*100 + 2), HeadSHA: proposal.HeadSHA,
			}
			observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
			observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
			mutate(&observation, review)
			if _, err := database.SettleAgentTurn(ctx, lease, observation); !errors.Is(err, store.ErrAgentTurnSettlementRejected) {
				t.Errorf("reducer rejection %d error = %v", index, err)
			}
			assertSettlementRolledBack(t, pool, ctx, fixture.workflowID, lease.ID, workflow.StateReviewing)
		}
	})

	t.Run("unsettled mutations reject settlement without writes", func(t *testing.T) {
		fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 808, workflow.RoleDeveloper, "")
		mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "unsettled-808", ToolName: "publish_changes", Request: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CloseMutationAdmission(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if _, err := database.SettleAgentTurn(ctx, lease, failedSettlementObservation("runtime failed")); !errors.Is(err, store.ErrAgentTurnMutationsUnsettled) {
			t.Errorf("settlement with mutation %s error = %v", mutation.ID, err)
		}
		assertSettlementRolledBack(t, pool, ctx, fixture.workflowID, lease.ID, workflow.StateDeveloping)
	})

	t.Run("stale owner fence rejects settlement", func(t *testing.T) {
		fixture, lease, _ := prepareSettlementTurn(t, database, pool, ctx, 809, workflow.RoleDeveloper, "")
		stale := lease
		stale.OwnerToken = "78090000-0000-4000-8000-000000000099"
		if _, err := database.SettleAgentTurn(ctx, stale, failedSettlementObservation("runtime failed")); !errors.Is(err, store.ErrAgentTurnFenceLost) {
			t.Errorf("stale settlement error = %v", err)
		}
		assertSettlementRolledBack(t, pool, ctx, fixture.workflowID, lease.ID, workflow.StateDeveloping)
	})

	t.Run("action failure rolls back proposal workflow settlement and finalization", func(t *testing.T) {
		fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 810, workflow.RoleDeveloper, "")
		if _, err := pool.Exec(ctx, `
INSERT INTO jobs (id, queue, kind, payload, status, max_attempts, idempotency_key,
                  workflow_id, workflow_attempt_id)
VALUES ('78100000-0000-4000-8000-000000000001', 'workflow', 'PREPARE_AGENT_TURN',
        '{}'::jsonb, 'AVAILABLE', 3, 'forced-live-successor', $1, $2)`,
			fixture.workflowID, fixture.attemptID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal)); err == nil {
			t.Fatal("SettleAgentTurn() action conflict error = nil")
		}
		assertSettlementRolledBack(t, pool, ctx, fixture.workflowID, lease.ID, workflow.StateDeveloping)
		var proposals int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM change_proposals WHERE workflow_id = $1`, fixture.workflowID).Scan(&proposals); err != nil {
			t.Fatal(err)
		}
		if proposals != 0 {
			t.Errorf("rolled-back initial Change Proposals = %d, want 0", proposals)
		}
	})
}

func TestPendingEventCausalReplayIsSerializedAcrossStores(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, turnLease, proposal := prepareSettlementTurn(t, databases[0], pool, ctx, 828, workflow.RoleReviewer, "head-1")
	baseTime := time.Date(2026, time.September, 4, 10, 9, 0, 0, time.UTC)
	for index, event := range []deferredSettlementEvent{
		{deliveryID: "78280000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize", beforeSHA: "head-3", headSHA: "head-4"},
		{deliveryID: "78280000-0000-4000-8000-000000000002", eventName: "pull_request", action: "synchronize", beforeSHA: "head-2", headSHA: "head-3"},
		{deliveryID: "78280000-0000-4000-8000-000000000003", eventName: "pull_request", action: "synchronize", beforeSHA: "head-1", headSHA: "head-2"},
	} {
		event.createdAt = baseTime.Add(time.Duration(index) * time.Millisecond)
		insertDeferredSettlementNormalizedEvent(t, pool, ctx, turnLease.JobLease.WorkflowID, turnLease.ID, event)
	}
	review := &workflow.ReviewIdentity{ID: 82801, NodeID: "PRR_828", ChangeProposalID: proposal.PullRequestID, ActorID: 82802, HeadSHA: "head-1"}
	observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
	observation.Review, observation.AuthorizedReviewerActorID = review, review.ActorID
	settled, err := databases[0].SettleAgentTurn(ctx, turnLease, observation)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "concurrent-causal-reconciler", 20*time.Second)
	if err != nil || lease == nil || lease.ID != settled.ReconciliationJobID {
		t.Fatalf("claim concurrent causal reconciliation = (%#v, %v)", lease, err)
	}
	type result struct {
		ack store.PendingEventReconciliation
		err error
	}
	start := make(chan struct{})
	results := make(chan result, len(databases))
	for _, database := range databases {
		go func(database *store.Store) {
			<-start
			ack, err := database.AcknowledgePendingEventReconciliation(ctx, *lease, settlementPendingTransitionFactory)
			results <- result{ack: ack, err: err}
		}(database)
	}
	close(start)
	var succeeded, fenced int
	for range databases {
		result := <-results
		switch {
		case result.err == nil:
			succeeded++
			if result.ack.CompletedCount != 3 || result.ack.Successor.ExpectedHeadSHA != "head-4" {
				t.Errorf("concurrent causal acknowledgement = %#v", result.ack)
			}
		case errors.Is(result.err, store.ErrPendingEventReconciliationFenceLost):
			fenced++
		default:
			t.Fatalf("concurrent causal acknowledgement error = %v", result.err)
		}
	}
	var head string
	var completed, successors int
	if err := pool.QueryRow(ctx, `
SELECT proposal.head_sha,
       (SELECT count(*) FROM normalized_events WHERE workflow_id = $1 AND status = 'COMPLETED'),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'
           AND status IN ('AVAILABLE', 'LEASED'))
FROM change_proposals AS proposal WHERE proposal.workflow_id = $1 AND proposal.active`, fixture.workflowID).Scan(
		&head, &completed, &successors); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || fenced != 1 || head != "head-4" || completed != 3 || successors != 1 {
		t.Errorf("concurrent causal replay = succeeded %d fenced %d head %q completed %d successors %d",
			succeeded, fenced, head, completed, successors)
	}
}

func prepareSettlementTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, number int, role workflow.Role, head string) (agentFixture, store.AgentTurnLease, *store.AgentTurnSettlementChangeProposal) {
	t.Helper()
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, role, head)
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	return fixture, lease, proposal
}

func prepareOpenSettlementTurn(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, number int, role workflow.Role, head string) (agentFixture, store.AgentTurnLease, *store.AgentTurnSettlementChangeProposal) {
	t.Helper()
	fixture := seedAgentSession(t, pool, number)
	state := workflow.StateDeveloping
	if role == workflow.RoleReviewer {
		state = workflow.StateReviewing
	}
	if _, err := pool.Exec(ctx, `
UPDATE workflows SET status = $2, state_revision = 1,
    desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE'
WHERE id = $1`, fixture.workflowID, state); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE workflow_attempts SET infrastructure_failure_limit = 1,
    review_cycle_limit = 3 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	var proposal *store.AgentTurnSettlementChangeProposal
	proposalRowID := ""
	if role == workflow.RoleReviewer {
		actorID := int64(number*100 + 2)
		if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET role = 'REVIEWER', agent_profile_name = 'reviewer',
    github_app_actor_id = $2 WHERE id = $1`, fixture.assignmentID, actorID); err != nil {
			t.Fatal(err)
		}
		proposalRowID = fmt.Sprintf("79%04d00-0000-4000-8000-000000000001", number)
		proposal = settlementProposal(fixture, number, head)
		if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, pull_request_node_id, status, active,
    base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, $3, 'owner', 'repo', $4, $5, $6, 'OPEN', TRUE,
        'main', 'base-sha', 'feature', $7)`, proposalRowID, fixture.workflowID,
			proposal.RepositoryID, proposal.PullRequestID, proposal.PullRequestNumber,
			proposal.PullRequestNodeID, proposal.HeadSHA); err != nil {
			t.Fatal(err)
		}
	} else {
		proposal = settlementProposal(fixture, number, "developer-head")
	}
	spec := fixture.turnSpec()
	if role == workflow.RoleReviewer {
		spec.Purpose = workflow.TurnPurposeReview
		spec.ChangeProposalID = proposalRowID
		spec.ExpectedHeadSHA = head
		spec.AgentProfileConfig = agentProfileConfig("reviewer", workflow.RoleReviewer, "runtime/1", "provider/test", "", 10, "Review test instructions.", nil)
	}
	turn, err := database.AllocateAgentTurn(ctx, spec)
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, 20*time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "settlement-runtime", 20*time.Second, 100)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	return fixture, lease, proposal
}

func settlementProposal(fixture agentFixture, number int, head string) *store.AgentTurnSettlementChangeProposal {
	return &store.AgentTurnSettlementChangeProposal{
		WorkflowID: fixture.workflowID, RepositoryID: int64(number), RepositoryOwner: "owner", RepositoryName: "repo",
		PullRequestID: int64(number * 100), PullRequestNumber: int64(number),
		PullRequestNodeID: fmt.Sprintf("PR_%d", number), Status: "OPEN", Active: true,
		BaseRef: "main", BaseSHA: "base-sha", HeadRef: "feature", HeadSHA: head,
	}
}

func successfulSettlementObservation(outcome workflow.TurnOutcome, proposal *store.AgentTurnSettlementChangeProposal) store.AgentTurnSettlementObservation {
	return store.AgentTurnSettlementObservation{
		ObservedAt: time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC),
		Outcome:    outcome, ChangeProposal: proposal,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: json.RawMessage(`{"terminal":true}`)},
	}
}

func failedSettlementObservation(diagnostic string) store.AgentTurnSettlementObservation {
	return store.AgentTurnSettlementObservation{
		ObservedAt: time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC),
		Outcome:    workflow.TurnOutcomeInfrastructureFailed, Diagnostic: diagnostic,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: diagnostic},
	}
}

func insertDeferredSettlementEvent(t *testing.T, pool *pgxpool.Pool, ctx context.Context, deliveryID, workflowID, turnID, head string) {
	t.Helper()
	insertDeferredSettlementNormalizedEvent(t, pool, ctx, workflowID, turnID, deferredSettlementEvent{
		deliveryID: deliveryID, eventName: "pull_request_review", action: "submitted",
		headSHA: head, createdAt: time.Now().UTC(),
	})
}

type deferredSettlementEvent struct {
	deliveryID string
	eventName  string
	action     string
	beforeSHA  string
	headSHA    string
	createdAt  time.Time
}

func insertDeferredSettlementNormalizedEvent(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID, turnID string, event deferredSettlementEvent) {
	t.Helper()
	var repositoryID int64
	var repositoryOwner, repositoryName string
	if err := pool.QueryRow(ctx, `SELECT repository_id, repository_owner, repository_name FROM workflows WHERE id = $1`, workflowID).Scan(&repositoryID, &repositoryOwner, &repositoryName); err != nil {
		t.Fatal(err)
	}
	normalized := map[string]any{
		"delivery_id": event.deliveryID,
		"event":       event.eventName,
		"action":      event.action,
		"repository": map[string]any{
			"id": repositoryID, "owner": repositoryOwner, "name": repositoryName,
		},
		"pull_request": map[string]any{
			"id": repositoryID * 100, "number": repositoryID, "base_ref": "main", "base_sha": "base-sha",
			"head_ref": "feature", "head_sha": event.headSHA, "before_sha": event.beforeSHA,
		},
	}
	if event.eventName == "pull_request_review" {
		normalized["review"] = map[string]any{
			"id": repositoryID*100 + 99, "node_id": fmt.Sprintf("DEFERRED_PRR_%d", repositoryID),
			"state": "approved", "commit_id": event.headSHA,
			"user": map[string]any{"id": repositoryID*100 + 98, "login": "reviewer"},
		}
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO webhook_deliveries (
    delivery_id, event_name, action, repository_id, repository_owner,
    repository_name, headers, payload, status, processed_at
)
VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb,
		'{}'::bytea, 'PROCESSED', clock_timestamp())`, event.deliveryID, event.eventName, event.action,
		repositoryID, repositoryOwner, repositoryName); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO normalized_events (
    delivery_id, payload, status, workflow_id, disposition, reason,
    applied_revision, deferred_for_turn_id, processed_at, created_at
)
VALUES ($1, $2, 'DEFERRED', $3, 'DEFERRED', 'active_turn', 1, $4,
		clock_timestamp(), $5)`, event.deliveryID, payload, workflowID, turnID, event.createdAt); err != nil {
		t.Fatal(err)
	}
}

func settlementPendingTransitionFactory(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
	var event struct {
		EventName  string `json:"event"`
		Action     string `json:"action"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
		PullRequest struct {
			ID        int64  `json:"id"`
			Number    int64  `json:"number"`
			BeforeSHA string `json:"before_sha"`
			HeadSHA   string `json:"head_sha"`
		} `json:"pull_request"`
		Review struct {
			ID       int64  `json:"id"`
			NodeID   string `json:"node_id"`
			CommitID string `json:"commit_id"`
			User     struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"review"`
	}
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		return store.WorkflowLocator{}, nil, err
	}
	locator := store.WorkflowLocator{RepositoryID: event.Repository.ID, PullRequestID: event.PullRequest.ID}
	transition := func(snapshot workflow.Snapshot) workflow.Decision {
		metadata := workflow.EventMetadata{ID: record.DeliveryID, ObservedAt: record.CreatedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision}
		switch event.EventName + "." + event.Action {
		case "pull_request.synchronize":
			return workflow.Reduce(snapshot, workflow.SynchronizationEvent{
				EventMetadata: metadata, ChangeProposalID: event.PullRequest.ID,
				PreviousHeadSHA: event.PullRequest.BeforeSHA, HeadSHA: event.PullRequest.HeadSHA,
			})
		case "pull_request.opened":
			return workflow.Reduce(snapshot, workflow.ChangeProposalObservedEvent{
				EventMetadata:  metadata,
				ChangeProposal: workflow.ChangeProposal{ID: event.PullRequest.ID, Number: event.PullRequest.Number, HeadSHA: event.PullRequest.HeadSHA, Open: true},
			})
		case "pull_request_review.submitted":
			return workflow.Reduce(snapshot, workflow.ReviewObservedEvent{
				EventMetadata: metadata,
				Review: workflow.ReviewIdentity{
					ID: event.Review.ID, NodeID: event.Review.NodeID, ChangeProposalID: event.PullRequest.ID,
					ActorID: event.Review.User.ID, HeadSHA: event.Review.CommitID,
				},
			})
		default:
			return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionIllegal, Reason: workflow.ReasonInvalidEvent}
		}
	}
	return locator, transition, nil
}

func assertSettledExecution(t *testing.T, pool *pgxpool.Pool, ctx context.Context, lease store.AgentTurnLease, settled store.AgentTurnSettlement) {
	t.Helper()
	var turnStatus, jobStatus, attemptStatus string
	var turnActive bool
	var slots int
	if err := pool.QueryRow(ctx, `
SELECT turn.status, turn.active, job.status, attempt.status,
       (SELECT count(*) FROM agent_turn_slots WHERE agent_turn_id = turn.id)
FROM agent_turns AS turn
JOIN jobs AS job ON job.id = $2
JOIN job_attempts AS attempt ON attempt.job_id = job.id AND attempt.attempt_number = $3
WHERE turn.id = $1`, lease.ID, lease.JobLease.ID, lease.JobLease.Attempt).Scan(
		&turnStatus, &turnActive, &jobStatus, &attemptStatus, &slots); err != nil {
		t.Fatal(err)
	}
	if turnStatus != string(settled.TerminalStatus) || turnActive || jobStatus != string(store.JobSucceeded) || attemptStatus != string(store.JobSucceeded) || slots != 0 {
		t.Errorf("settled execution = turn %s/%t, job %s, attempt %s, slots %d", turnStatus, turnActive, jobStatus, attemptStatus, slots)
	}
}

func assertSettlementRolledBack(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID, turnID string, state workflow.State) {
	t.Helper()
	var storedState, turnStatus, jobStatus string
	var active bool
	var settlements int
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, turn.status, turn.active, job.status,
       (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = turn.id)
FROM workflows AS workflow
JOIN agent_turns AS turn ON turn.workflow_id = workflow.id AND turn.id = $2
JOIN jobs AS job ON job.agent_turn_id = turn.id AND job.kind = 'RUN_AGENT_TURN'
WHERE workflow.id = $1`, workflowID, turnID).Scan(&storedState, &turnStatus, &active, &jobStatus, &settlements); err != nil {
		t.Fatal(err)
	}
	if storedState != string(state) || turnStatus != string(store.AgentTurnSettling) || !active || jobStatus != string(store.JobLeased) || settlements != 0 {
		t.Errorf("rollback = Workflow %s, Turn %s/%t, Job %s, settlements %d", storedState, turnStatus, active, jobStatus, settlements)
	}
}
