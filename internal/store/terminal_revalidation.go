package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

// SettleTerminalRevalidation applies old terminal evidence to a new
// human-triggered Workflow Attempt through its own internal event. It never
// edits the old Turn or its failed settlement.
func (store *Store) SettleTerminalRevalidation(ctx context.Context, lease JobLease, expectedRevision int64, observation AgentTurnSettlementObservation) error {
	canonical, _, observationHash, err := canonicalizeAgentTurnSettlementObservation(observation)
	if err != nil {
		return err
	}
	if canonical.Outcome != workflow.TurnOutcomeChangeProposalReady &&
		canonical.Outcome != workflow.TurnOutcomeApproved && canonical.Outcome != workflow.TurnOutcomeChangesRequested {
		return ErrAgentTurnSettlementInvalid
	}
	return store.applyTerminalRevalidation(ctx, lease, canonical, observationHash, "", "", expectedRevision)
}

func (store *Store) CompleteTerminalRevalidationHandoff(ctx context.Context, lease JobLease, reason workflow.Reason, failureCode string) error {
	if (reason != workflow.ReasonTerminalCorroborationExhausted && reason != workflow.ReasonTerminalCorroborationPrerequisite) ||
		!validCorroborationFailureCode(failureCode) {
		return ErrTerminalCorroborationConflict
	}
	diagnostic := "Previously successful terminal mutation still cannot be corroborated: " + failureCode
	return store.applyTerminalRevalidation(ctx, lease, canonicalSettlementObservation{}, [32]byte{}, reason, diagnostic, 0)
}

func (store *Store) applyTerminalRevalidation(ctx context.Context, lease JobLease, canonical canonicalSettlementObservation, observationHash [32]byte,
	handoffReason workflow.Reason, diagnostic string, expectedRevision int64) error {
	if lease.Kind != RevalidateTerminalIntentJobKind || lease.Queue != "agent-turn-recovery" ||
		!validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 ||
		!validUUID(lease.WorkflowAttemptID) || !validUUID(lease.AgentTurnID) {
		return ErrJobLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workflowStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`,
		lease.WorkflowID).Scan(&workflowStatus); err != nil {
		return corroborationReadError(err)
	}
	if handoffReason == "" && workflowStatus != "DEVELOPING" && workflowStatus != "REVIEWING" {
		return ErrJobLeaseLost
	}
	var priorPayload []byte
	var priorKind string
	err = tx.QueryRow(ctx, `
SELECT kind, payload FROM workflow_internal_events
WHERE source_job_id = $1 AND source_attempt_number = $2 AND source_lease_token = $3
  AND applied_at IS NOT NULL`, lease.ID, lease.Attempt, lease.LeaseToken).Scan(&priorKind, &priorPayload)
	if err == nil {
		var prior struct {
			ObservationHash string `json:"observation_hash"`
			HandoffReason   string `json:"handoff_reason"`
		}
		if json.Unmarshal(priorPayload, &prior) != nil ||
			priorKind == string(workflow.EventKindTerminalIntentRevalidated) && (handoffReason != "" || prior.ObservationHash != fmt.Sprintf("%x", observationHash)) ||
			priorKind == string(workflow.EventKindTerminalRevalidationFailed) && (handoffReason == "" || prior.HandoffReason != string(handoffReason)) {
			return ErrTerminalCorroborationConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return corroborationReadError(err)
	}
	if job.Kind != RevalidateTerminalIntentJobKind || job.Queue != "agent-turn-recovery" ||
		job.Status != JobLeased || job.LeaseOwner != lease.LeaseOwner || job.LeaseToken != lease.LeaseToken ||
		job.AttemptCount != lease.Attempt || job.WorkflowID != lease.WorkflowID ||
		job.WorkflowAttemptID != lease.WorkflowAttemptID || job.AgentAssignmentID != lease.AgentAssignmentID ||
		job.AgentSessionID != lease.AgentSessionID || job.AgentTurnID != lease.AgentTurnID ||
		job.ExecutionEpoch != lease.ExecutionEpoch {
		return ErrJobLeaseLost
	}
	var jobLive, attemptLive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil {
		return corroborationReadError(err)
	}
	if !jobLive {
		return ErrJobLeaseLost
	}
	if err := tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_owner = $4 AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, job.AttemptCount, job.LeaseToken, job.LeaseOwner).Scan(&attemptLive); err != nil {
		return corroborationReadError(err)
	}
	if !attemptLive {
		return ErrJobLeaseLost
	}
	var sourceID, promptError string
	var promptOutcome []byte
	if err := tx.QueryRow(ctx, `
SELECT source_invocation_id::text, prompt_outcome, COALESCE(prompt_error, '')
FROM agent_turn_corroborations WHERE agent_turn_id = $1 AND workflow_id = $2
  AND state = 'HANDED_OFF' FOR UPDATE`, job.AgentTurnID, job.WorkflowID).Scan(
		&sourceID, &promptOutcome, &promptError); err != nil {
		return corroborationReadError(err)
	}
	var payload struct {
		SourceTurnID string `json:"source_turn_id"`
	}
	if json.Unmarshal(job.Payload, &payload) != nil || payload.SourceTurnID != job.AgentTurnID {
		return ErrJobLeaseLost
	}
	turn, err := lockAgentTurn(ctx, tx, job.AgentTurnID)
	if err != nil {
		return corroborationReadError(err)
	}
	if turn.active || turn.Status != AgentTurnFailed || turn.AgentSessionID != job.AgentSessionID ||
		turn.ExecutionEpoch != job.ExecutionEpoch || turn.WorkflowAttemptID == job.WorkflowAttemptID {
		return ErrJobLeaseLost
	}
	var sessionOwner SessionControlOwner
	if err := tx.QueryRow(ctx, `SELECT control_owner FROM agent_sessions WHERE id = $1`,
		job.AgentSessionID).Scan(&sessionOwner); err != nil {
		return corroborationReadError(err)
	}
	if sessionOwner != SessionControlAutomation {
		return ErrJobLeaseLost
	}
	turn.AgentAssignmentID, turn.AgentParticipantID = job.AgentAssignmentID, job.AgentAssignmentID
	var role workflow.Role
	if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments
WHERE id = $1 AND workflow_id = $2 AND status = 'ACTIVE'`, job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
		return corroborationReadError(err)
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return err
	}
	if snapshot.CurrentAttempt == nil || snapshot.CurrentAttempt.ID != job.WorkflowAttemptID || snapshot.ActiveTurn != nil {
		return ErrJobLeaseLost
	}
	if handoffReason == "" && int64(snapshot.Revision) != expectedRevision {
		return ErrTerminalCorroborationHeadMoved
	}
	compatible := store.reducer.DefinitionCompatible(snapshot)
	if compatible && snapshot.CurrentAttempt.CurrentStage != turn.Stage {
		return ErrJobLeaseLost
	}
	if !compatible && handoffReason == "" {
		return ErrAgentTurnSettlementRejected
	}
	eventID, err := randomUUID()
	if err != nil {
		return err
	}
	var decision workflow.Decision
	var eventKind workflow.EventKind
	if !compatible {
		eventKind = workflow.EventKindTerminalRevalidationFailed
		decision = store.reducer.CorroborationDefinitionIncompatible(snapshot)
	} else if handoffReason != "" {
		eventKind = workflow.EventKindTerminalRevalidationFailed
		decision = store.reducer.Reduce(snapshot, workflow.TerminalRevalidationFailedEvent{
			EventMetadata: workflow.EventMetadata{ID: eventID, ObservedAt: job.CreatedAt,
				WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
			AttemptID: job.WorkflowAttemptID, SourceTurnID: turn.ID,
			Reason: handoffReason, Diagnostic: diagnostic,
		})
	} else {
		if promptError == "" {
			checkpointOutcome, err := canonicalJSON(promptOutcome)
			if err != nil || !bytes.Equal(canonical.TerminalOutcome, checkpointOutcome) {
				return ErrTerminalCorroborationConflict
			}
		} else if len(canonical.TerminalOutcome) != 0 ||
			promptError == "FAILURE" && !strings.HasPrefix(canonical.Diagnostic, "ACP prompt failed") ||
			promptError == "DEADLINE" && !strings.HasPrefix(canonical.Diagnostic, "ACP prompt deadline exceeded") {
			return ErrTerminalCorroborationConflict
		}
		// A synchronization may have committed after the external read. Do not
		// overwrite its newer durable head with an older observation.
		if snapshot.ChangeProposal != nil && canonical.ChangeProposal != nil &&
			canonical.ChangeProposal.HeadSHA != snapshot.ChangeProposal.HeadSHA &&
			!(role == workflow.RoleReviewer && canonical.ChangeProposal.HeadSHA != turn.ExpectedHeadSHA) {
			return ErrTerminalCorroborationHeadMoved
		}
		if canonical.ExistingReview != nil {
			var accepted bool
			if err := tx.QueryRow(ctx, `
SELECT accepted FROM change_proposal_reviews WHERE repository_id = $1 AND review_id = $2
  AND review_node_id = $3`, canonical.ChangeProposal.RepositoryID,
				canonical.ExistingReview.ID, canonical.ExistingReview.NodeID).Scan(&accepted); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrTerminalCorroborationConflict
				}
				return err
			}
			if !accepted {
				return ErrTerminalCorroborationConflict
			}
		}
		proposalID, proposal, err := store.validateSettlementChangeProposal(ctx, tx, job, turn, role, snapshot, canonical)
		if err != nil {
			return err
		}
		if err := store.validateSettlementReviewerActor(ctx, tx, job, role, canonical); err != nil {
			return err
		}
		if err := validateTerminalCorroborationSource(ctx, tx, job, sourceID, role, canonical); err != nil {
			return err
		}
		pending, err := derivePendingEventsObservation(ctx, tx, job.WorkflowID, turn.ID)
		if err != nil || pending.Count != 0 {
			return ErrAgentTurnMutationsUnsettled
		}
		eventKind = workflow.EventKindTerminalIntentRevalidated
		decision = store.reducer.Reduce(snapshot, workflow.TerminalIntentRevalidatedEvent{
			EventMetadata: workflow.EventMetadata{ID: eventID, ObservedAt: canonical.ObservedAt,
				WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
			AttemptID: job.WorkflowAttemptID,
			SourceTurn: workflow.TurnGuard{TurnID: turn.ID, SessionID: turn.AgentSessionID,
				AttemptID: turn.WorkflowAttemptID, Stage: turn.Stage, Role: role,
				Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
				ChangeProposalID: func() int64 {
					if snapshot.ChangeProposal != nil {
						return snapshot.ChangeProposal.ID
					}
					return 0
				}(),
				ExpectedHeadSHA: turn.ExpectedHeadSHA},
			Outcome: canonical.Outcome, ChangeProposal: proposal, Review: canonical.Review,
			ExistingReview: canonical.ExistingReview, AuthorizedReviewerActorID: canonical.AuthorizedReviewerActorID,
		})
		if decision.Disposition == workflow.DispositionApplied && proposalID == "" && canonical.ChangeProposal != nil {
			if _, err := insertInitialSettlementChangeProposal(ctx, tx, eventID, job, turn, *canonical.ChangeProposal); err != nil {
				return err
			}
		}
	}
	if err := validateWorkflowDecision(snapshot, decision); err != nil || decision.Disposition != workflow.DispositionApplied {
		return ErrAgentTurnSettlementRejected
	}
	payloadJSON, _ := json.Marshal(map[string]string{
		"source_turn_id": turn.ID, "source_invocation_id": sourceID,
		"observation_hash": fmt.Sprintf("%x", observationHash), "handoff_reason": string(handoffReason),
	})
	if err := insertInternalEventTx(ctx, tx, eventID, job, eventKind, payloadJSON, job.CreatedAt); err != nil {
		return err
	}
	if err := persistAppliedDecisionWithInternalProvenance(ctx, tx, "", "", eventID, job.WorkflowID, snapshot, decision, ""); err != nil {
		return err
	}
	if err := completeInternalEventTx(ctx, tx, eventID, decision); err != nil {
		return err
	}
	resultJSON, _ := json.Marshal(map[string]string{"internal_event_id": eventID})
	attemptUpdated, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, resultJSON)
	if err != nil || attemptUpdated.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	jobUpdated, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    updated_at = clock_timestamp(), completed_at = clock_timestamp(), result = $4
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, resultJSON)
	if err != nil || jobUpdated.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	return tx.Commit(ctx)
}
