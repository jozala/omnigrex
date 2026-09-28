package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

// SettleTerminalCorroboration applies an original Turn's proven outcome under
// the exact leased verifier authority. The completed execution job is never
// reopened, and the Runtime Process has already stopped.
func (store *Store) SettleTerminalCorroboration(ctx context.Context, lease JobLease, observation AgentTurnSettlementObservation) (AgentTurnSettlement, error) {
	return store.settleTerminalCorroboration(ctx, lease, observation, "")
}

// CompleteTerminalCorroborationHandoff stops automation without consuming an
// Agent Turn infrastructure retry. The failure code is a fixed safe category.
func (store *Store) CompleteTerminalCorroborationHandoff(ctx context.Context, lease JobLease, reason workflow.Reason, failureCode string) (AgentTurnSettlement, error) {
	if (reason != workflow.ReasonTerminalCorroborationExhausted && reason != workflow.ReasonTerminalCorroborationPrerequisite) ||
		!validCorroborationFailureCode(failureCode) || lease.LeasedAt == nil {
		return AgentTurnSettlement{}, ErrTerminalCorroborationConflict
	}
	diagnostic := "Terminal mutation succeeded but fresh corroboration was unavailable: " + failureCode
	return store.settleTerminalCorroboration(ctx, lease, AgentTurnSettlementObservation{
		ObservedAt: *lease.LeasedAt, Outcome: workflow.TurnOutcomeInfrastructureFailed,
		Diagnostic: diagnostic, Completion: AgentTurnCompletion{Status: AgentTurnFailed, LastError: diagnostic},
	}, reason)
}

func (store *Store) settleTerminalCorroboration(ctx context.Context, lease JobLease, observation AgentTurnSettlementObservation, handoffReason workflow.Reason) (AgentTurnSettlement, error) {
	canonical, observationJSON, observationHash, err := canonicalizeAgentTurnSettlementObservation(observation)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	if handoffReason == "" && canonical.Outcome != workflow.TurnOutcomeChangeProposalReady &&
		canonical.Outcome != workflow.TurnOutcomeChangesRequested && canonical.Outcome != workflow.TurnOutcomeApproved {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementInvalid
	}
	if handoffReason != "" && (canonical.Outcome != workflow.TurnOutcomeInfrastructureFailed ||
		handoffReason != workflow.ReasonTerminalCorroborationExhausted && handoffReason != workflow.ReasonTerminalCorroborationPrerequisite) {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementInvalid
	}
	if lease.Kind != VerifyTerminalIntentJobKind || lease.Queue != "agent-turn-recovery" ||
		!validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"agent-turn-settlement:"+lease.AgentTurnID+":"+fmt.Sprint(lease.ExecutionEpoch)); err != nil {
		return AgentTurnSettlement{}, err
	}
	// Closure takes the Workflow row before its stop/settlement jobs and Turn.
	// Follow that order before locking the verifier job to avoid a cycle.
	var workflowStateForLock string
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`,
		lease.WorkflowID).Scan(&workflowStateForLock); err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	var priorJSON, priorHash []byte
	var priorAttempt int
	var priorToken string
	err = tx.QueryRow(ctx, `
SELECT result, observation_sha256, recovery_job_attempt_number, recovery_job_lease_token::text
FROM agent_turn_settlements WHERE recovery_job_id = $1 AND settled_at IS NOT NULL`, lease.ID).Scan(
		&priorJSON, &priorHash, &priorAttempt, &priorToken)
	if err == nil {
		if priorAttempt != lease.Attempt || priorToken != lease.LeaseToken || !bytes.Equal(priorHash, observationHash[:]) {
			return AgentTurnSettlement{}, ErrAgentTurnSettlementConflict
		}
		var prior AgentTurnSettlement
		if json.Unmarshal(priorJSON, &prior) != nil {
			return AgentTurnSettlement{}, ErrAgentTurnSettlementConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnSettlement{}, err
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnSettlement{}, err
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	if job.Kind != VerifyTerminalIntentJobKind || job.Status != JobLeased ||
		job.LeaseOwner != lease.LeaseOwner || job.LeaseToken != lease.LeaseToken ||
		job.AttemptCount != lease.Attempt || job.AgentTurnID != lease.AgentTurnID ||
		job.ExecutionEpoch != lease.ExecutionEpoch || job.WorkflowID != lease.WorkflowID ||
		job.WorkflowAttemptID != lease.WorkflowAttemptID || job.AgentAssignmentID != lease.AgentAssignmentID ||
		job.AgentSessionID != lease.AgentSessionID {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	var jobLive, attemptLive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	if !jobLive {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	if err := tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_owner = $4
       AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, job.AttemptCount, job.LeaseToken, job.LeaseOwner).Scan(&attemptLive); err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	if !attemptLive {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	var sourceID, executionJobID, originalOwnerID, promptError string
	var originalOwnerHash []byte
	var promptOutcome []byte
	err = tx.QueryRow(ctx, `
SELECT source_invocation_id::text, execution_job_id::text,
       execution_owner_id, execution_owner_token_sha256, prompt_outcome,
       COALESCE(prompt_error, '')
FROM agent_turn_corroborations
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND verification_job_id = $3
  AND workflow_attempt_id = $4 AND state = 'PENDING' FOR UPDATE`,
		job.AgentTurnID, job.ExecutionEpoch, job.ID, job.WorkflowAttemptID).Scan(
		&sourceID, &executionJobID, &originalOwnerID, &originalOwnerHash,
		&promptOutcome, &promptError)
	if err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	if sourceID == "" || originalOwnerID == "" || len(originalOwnerHash) != sha256.Size {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	if handoffReason == "" && promptError == "" {
		checkpointOutcome, err := canonicalJSON(promptOutcome)
		if err != nil || !bytes.Equal(canonical.TerminalOutcome, checkpointOutcome) {
			return AgentTurnSettlement{}, ErrTerminalCorroborationConflict
		}
	} else if handoffReason == "" {
		if len(canonical.TerminalOutcome) != 0 ||
			promptError == "FAILURE" && !strings.HasPrefix(canonical.Diagnostic, "ACP prompt failed") ||
			promptError == "DEADLINE" && !strings.HasPrefix(canonical.Diagnostic, "ACP prompt deadline exceeded") {
			return AgentTurnSettlement{}, ErrTerminalCorroborationConflict
		}
	}
	turn, err := lockAgentTurn(ctx, tx, job.AgentTurnID)
	if err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	if !turn.active || turn.Status != AgentTurnCorroborating || turn.MutationAdmissionOpen ||
		turn.WorkflowAttemptID != job.WorkflowAttemptID || turn.AgentSessionID != job.AgentSessionID ||
		turn.ExecutionEpoch != job.ExecutionEpoch {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	turn.AgentAssignmentID = job.AgentAssignmentID
	turn.AgentParticipantID = job.AgentAssignmentID
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, turn.ID, turn.ExecutionEpoch).Scan(&unsettled); err != nil {
		return AgentTurnSettlement{}, err
	}
	if unsettled {
		return AgentTurnSettlement{}, ErrAgentTurnMutationsUnsettled
	}
	var executionAttempt int
	var executionOwner, executionToken string
	if err := tx.QueryRow(ctx, `
SELECT execution.attempt_count, attempt.lease_owner, attempt.lease_token::text
FROM jobs AS execution JOIN job_attempts AS attempt
  ON attempt.job_id = execution.id AND attempt.attempt_number = execution.attempt_count
WHERE execution.id = $1 AND execution.kind = 'RUN_AGENT_TURN'
  AND execution.status = 'SUCCEEDED' AND attempt.status = 'SUCCEEDED'
  AND execution.agent_turn_id = $2 AND execution.execution_epoch = $3`,
		executionJobID, turn.ID, turn.ExecutionEpoch).Scan(&executionAttempt, &executionOwner, &executionToken); err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	var role workflow.Role
	if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments
WHERE id = $1 AND workflow_id = $2 AND status = 'ACTIVE'`,
		job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
		return AgentTurnSettlement{}, corroborationReadError(err)
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	if snapshot.ActiveTurn == nil || snapshot.ActiveTurn.ID != turn.ID ||
		snapshot.CurrentAttempt == nil || snapshot.CurrentAttempt.ID != job.WorkflowAttemptID {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	compatible := store.reducer.DefinitionCompatible(snapshot)
	if !compatible && handoffReason == "" {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementRejected
	}
	proposalID, proposal, err := store.validateSettlementChangeProposal(ctx, tx, job, turn, role, snapshot, canonical)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	if err := store.validateSettlementReviewerActor(ctx, tx, job, role, canonical); err != nil {
		return AgentTurnSettlement{}, err
	}
	if handoffReason == "" {
		if err := validateTerminalCorroborationSource(ctx, tx, job, sourceID, role, canonical); err != nil {
			return AgentTurnSettlement{}, err
		}
	}
	pending, err := derivePendingEventsObservation(ctx, tx, job.WorkflowID, turn.ID)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	settlementID, err := randomUUID()
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	event := workflow.TurnSettledEvent{
		EventMetadata: workflow.EventMetadata{
			ID: settlementID, ObservedAt: canonical.ObservedAt,
			WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
		},
		Turn: workflow.TurnGuard{
			TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
			Stage: turn.Stage, Role: role, Epoch: uint64(turn.ExecutionEpoch),
			ControlRevision:  uint64(turn.ControlRevision),
			ChangeProposalID: snapshot.ActiveTurn.ChangeProposalID,
			ExpectedHeadSHA:  turn.ExpectedHeadSHA,
		},
		Outcome: canonical.Outcome, ChangeProposal: proposal,
		Review: canonical.Review, ExistingReview: canonical.ExistingReview,
		AuthorizedReviewerActorID: canonical.AuthorizedReviewerActorID,
		PendingEvents:             pending, Diagnostic: canonical.Diagnostic,
		CorroborationHandoffReason: handoffReason,
	}
	decision := store.reducer.CorroborationDefinitionIncompatible(snapshot)
	if compatible {
		decision = store.reducer.Reduce(snapshot, event)
	}
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return AgentTurnSettlement{}, err
	}
	if decision.Disposition != workflow.DispositionApplied {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementRejected
	}
	if snapshot.ChangeProposal == nil && canonical.ChangeProposal != nil {
		proposalID, err = insertInitialSettlementChangeProposal(ctx, tx, settlementID, job, turn, *canonical.ChangeProposal)
		if err != nil {
			return AgentTurnSettlement{}, err
		}
	}
	_, err = tx.Exec(ctx, `
INSERT INTO agent_turn_settlements (
    id, workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, control_revision, authority, execution_job_id,
    job_attempt_number, job_lease_owner, job_lease_token, owner_id, owner_token_sha256,
    recovery_job_id, recovery_job_kind, recovery_job_attempt_number,
    recovery_job_lease_owner, recovery_job_lease_token,
    observation, observation_sha256, workflow_outcome, terminal_turn_status,
    terminal_turn_outcome, terminal_last_error, pending_event_count,
    latest_observed_head_sha, disposition, reason, workflow_state,
    workflow_revision, change_proposal_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'RECOVERY', $9, $10, $11, $12, $13,
        $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25,
        $26, $27, $28, $29, $30, $31, $32)`,
		settlementID, job.WorkflowID, job.WorkflowAttemptID, job.AgentAssignmentID,
		job.AgentSessionID, turn.ID, turn.ExecutionEpoch, turn.ControlRevision,
		executionJobID, executionAttempt, executionOwner, executionToken,
		originalOwnerID, originalOwnerHash, job.ID, job.Kind, job.AttemptCount,
		job.LeaseOwner, job.LeaseToken, observationJSON, observationHash[:],
		canonical.Outcome, canonical.TerminalStatus, nullableJSON(canonical.TerminalOutcome),
		nullableString(canonical.TerminalLastError), int64(pending.Count),
		nullableString(pending.LatestObservedHeadSHA), decision.Disposition, decision.Reason,
		decision.Snapshot.State, int64(decision.Snapshot.Revision), nullableString(proposalID))
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("persist verified terminal intent: %w", err)
	}
	if err := persistAppliedSettlementDecision(ctx, tx, settlementID, job.WorkflowID, snapshot, decision); err != nil {
		return AgentTurnSettlement{}, err
	}
	result := AgentTurnSettlement{
		ID: settlementID, WorkflowID: job.WorkflowID, AgentTurnID: turn.ID,
		ExecutionEpoch: turn.ExecutionEpoch, Authority: AgentTurnSettlementRecovery,
		RecoveryJobID: job.ID, RecoveryJobKind: job.Kind,
		Disposition: decision.Disposition, Reason: decision.Reason,
		State: decision.Snapshot.State, Revision: decision.Snapshot.Revision,
		TerminalStatus: canonical.TerminalStatus, ChangeProposalID: proposalID,
		PendingEventCount: pending.Count, LatestObservedHeadSHA: pending.LatestObservedHeadSHA,
	}
	if err := resolveSettlementActionJobs(ctx, tx, settlementID, &result); err != nil {
		return AgentTurnSettlement{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&result.SettledAt); err != nil {
		return AgentTurnSettlement{}, err
	}
	result.SettledAt = result.SettledAt.UTC()
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	jobResult := `{"terminal_intent_verified":true}`
	checkpointState := "VERIFIED"
	if handoffReason != "" {
		jobResult = `{"terminal_corroboration_handoff":true}`
		checkpointState = "HANDED_OFF"
	}
	attemptUpdate, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, jobResult)
	if err != nil || attemptUpdate.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	jobUpdate, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    updated_at = clock_timestamp(), completed_at = clock_timestamp(), result = $4
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, jobResult)
	if err != nil || jobUpdate.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	turnUpdate, err := tx.Exec(ctx, `
UPDATE agent_turns SET status = $3, active = FALSE, completed_at = clock_timestamp(),
    outcome = $4, last_error = $5
WHERE id = $1 AND execution_epoch = $2 AND status = 'CORROBORATING'
  AND active AND NOT mutation_admission_open`, turn.ID, turn.ExecutionEpoch,
		canonical.TerminalStatus, nullableJSON(canonical.TerminalOutcome), nullableString(canonical.TerminalLastError))
	if err != nil || turnUpdate.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	checkpointUpdate, err := tx.Exec(ctx, `
UPDATE agent_turn_corroborations SET state = $3, resolved_at = clock_timestamp()
WHERE agent_turn_id = $1 AND verification_job_id = $2 AND state = 'PENDING'`,
		turn.ID, job.ID, checkpointState)
	if err != nil || checkpointUpdate.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrJobLeaseLost
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turn_settlements SET successor_job_id = $2, reconciliation_job_id = $3,
    result = $4, settled_at = $5 WHERE id = $1 AND settled_at IS NULL`,
		settlementID, nullableString(result.SuccessorJobID), nullableString(result.ReconciliationJobID), resultJSON, result.SettledAt)
	if err != nil || updated.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnSettlement{}, err
	}
	return result, nil
}

func validateTerminalCorroborationSource(ctx context.Context, tx pgx.Tx, job Job, sourceID string, role workflow.Role, observation canonicalSettlementObservation) error {
	var tool, operation, service, resource, expectedSHA string
	var request, result []byte
	err := tx.QueryRow(ctx, `
SELECT tool_name, operation_id, request, result, COALESCE(external_service, ''),
       COALESCE(external_resource_id, ''), COALESCE(expected_sha, '')
FROM tool_invocations
WHERE id = $1 AND kind = 'MUTATION' AND state = 'SUCCEEDED'
  AND ((agent_turn_id = $2 AND execution_epoch = $3)
       OR EXISTS (
           SELECT 1 FROM tool_invocation_replays AS replay
           JOIN agent_turns AS original_turn ON original_turn.id = replay.source_agent_turn_id
           WHERE replay.agent_turn_id = $2 AND replay.execution_epoch = $3
             AND replay.source_tool_invocation_id = tool_invocations.id
             AND replay.source_agent_turn_id = tool_invocations.agent_turn_id
             AND (original_turn.outcome IS NULL OR original_turn.outcome->>'stop_reason' = 'end_turn')
             AND (original_turn.last_error IS NULL OR
                 (original_turn.last_error NOT LIKE 'ACP prompt was cancelled%'
                  AND original_turn.last_error NOT LIKE 'ACP prompt returned an invalid stop reason%'))
       ))`, sourceID, job.AgentTurnID, job.ExecutionEpoch).Scan(
		&tool, &operation, &request, &result, &service, &resource, &expectedSHA)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTerminalCorroborationConflict
		}
		return err
	}
	if operation == "" || observation.ChangeProposal == nil {
		return ErrTerminalCorroborationConflict
	}
	if tool == "confirm_prior_terminal_intent" {
		var confirmRequest struct {
			OperationID        string `json:"operation_id"`
			SourceInvocationID string `json:"source_invocation_id"`
		}
		var confirmed struct {
			SourceInvocationID string          `json:"source_invocation_id"`
			SourceTool         string          `json:"source_tool"`
			SourceOperationID  string          `json:"source_operation_id"`
			SourceRequest      json.RawMessage `json:"source_request"`
			SourceResult       json.RawMessage `json:"source_result"`
			ExternalService    string          `json:"external_service"`
			ExternalResourceID string          `json:"external_resource_id"`
			ExpectedSHA        string          `json:"expected_sha"`
		}
		if !decodeCorroborationObject(request, &confirmRequest) || !decodeCorroborationObject(result, &confirmed) ||
			confirmRequest.OperationID != operation || confirmRequest.SourceInvocationID != confirmed.SourceInvocationID ||
			confirmed.SourceInvocationID == "" || service != "omnigrex" || resource != job.WorkflowID ||
			expectedSHA != confirmed.ExpectedSHA {
			return ErrTerminalCorroborationConflict
		}
		var sourceTurnID, sourceSessionID, sourceAttemptID, sourceLineage, currentLineage string
		var priorRequest, priorResult []byte
		var priorTool, priorOperation, priorService, priorResource, priorHead string
		if err := tx.QueryRow(ctx, `
SELECT source_turn.id::text, source_turn.agent_session_id::text,
       source_turn.workflow_attempt_id::text, source_turn.operation_lineage_id::text,
       current_turn.operation_lineage_id::text, mutation.tool_name, mutation.operation_id,
       mutation.request, mutation.result, COALESCE(mutation.external_service, ''),
       COALESCE(mutation.external_resource_id, ''), COALESCE(mutation.expected_sha, '')
FROM tool_invocations AS mutation
JOIN agent_turns AS source_turn ON source_turn.id = mutation.agent_turn_id
JOIN agent_turns AS current_turn ON current_turn.id = $2
WHERE mutation.id = $1 AND mutation.kind = 'MUTATION' AND mutation.state = 'SUCCEEDED'
  AND source_turn.workflow_id = current_turn.workflow_id
  AND source_turn.status IN ('FAILED', 'INTERRUPTED', 'TIMED_OUT')
  AND (source_turn.outcome IS NULL OR source_turn.outcome->>'stop_reason' = 'end_turn')
  AND (source_turn.last_error IS NULL OR (source_turn.last_error NOT LIKE 'ACP prompt was cancelled%'
       AND source_turn.last_error NOT LIKE 'ACP prompt returned an invalid stop reason%'))`,
			confirmed.SourceInvocationID, job.AgentTurnID).Scan(
			&sourceTurnID, &sourceSessionID, &sourceAttemptID, &sourceLineage, &currentLineage,
			&priorTool, &priorOperation, &priorRequest, &priorResult,
			&priorService, &priorResource, &priorHead); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTerminalCorroborationConflict
			}
			return err
		}
		if sourceSessionID != job.AgentSessionID || sourceAttemptID != job.WorkflowAttemptID ||
			sourceLineage != currentLineage || priorTool != confirmed.SourceTool ||
			priorOperation != confirmed.SourceOperationID || priorService != confirmed.ExternalService ||
			priorResource != confirmed.ExternalResourceID || priorHead != confirmed.ExpectedSHA {
			return ErrTerminalCorroborationConflict
		}
		originalRequest, requestErr := canonicalJSON(priorRequest)
		confirmedRequest, confirmedRequestErr := canonicalJSON(confirmed.SourceRequest)
		originalResult, resultErr := canonicalJSON(priorResult)
		confirmedResult, confirmedResultErr := canonicalJSON(confirmed.SourceResult)
		if requestErr != nil || confirmedRequestErr != nil || resultErr != nil || confirmedResultErr != nil ||
			!bytes.Equal(originalRequest, confirmedRequest) || !bytes.Equal(originalResult, confirmedResult) {
			return ErrTerminalCorroborationConflict
		}
		if err := validateMutationReplayAncestor(ctx, tx, job.AgentTurnID, sourceTurnID); err != nil {
			return err
		}
		tool, operation, request, result = priorTool, priorOperation, priorRequest, priorResult
		service, resource, expectedSHA = priorService, priorResource, priorHead
	}
	proposal := observation.ChangeProposal
	switch role {
	case workflow.RoleDeveloper:
		var arguments struct {
			OperationID string `json:"operation_id"`
			Summary     string `json:"summary"`
		}
		var evidence struct {
			Outcome           string `json:"outcome"`
			PullRequestID     int64  `json:"pull_request_id"`
			PullRequestNumber int64  `json:"pull_request_number"`
			HeadSHA           string `json:"head_sha"`
		}
		if tool != "request_review" || service != "omnigrex" ||
			resource != strconv.FormatInt(proposal.RepositoryID, 10)+":"+proposal.HeadRef ||
			expectedSHA != proposal.HeadSHA ||
			!decodeCorroborationObject(request, &arguments) || arguments.OperationID != operation || arguments.Summary == "" ||
			!decodeCorroborationObject(result, &evidence) || evidence.Outcome != "REVIEW_REQUESTED" ||
			evidence.PullRequestID != proposal.PullRequestID || evidence.PullRequestNumber != proposal.PullRequestNumber ||
			evidence.HeadSHA != proposal.HeadSHA || observation.Outcome != workflow.TurnOutcomeChangeProposalReady {
			return ErrTerminalCorroborationConflict
		}
	case workflow.RoleReviewer:
		var arguments struct {
			OperationID string          `json:"operation_id"`
			Event       string          `json:"event"`
			Body        string          `json:"body"`
			Comments    json.RawMessage `json:"comments"`
			Signature   json.RawMessage `json:"signature"`
		}
		var evidence struct {
			ReviewID int64  `json:"review_id"`
			NodeID   string `json:"node_id"`
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			ActorID  int64  `json:"actor_id"`
			HTMLURL  string `json:"html_url"`
		}
		if tool != "submit_review" || service != "github" ||
			resource != strconv.FormatInt(proposal.RepositoryID, 10)+":"+strconv.FormatInt(proposal.PullRequestID, 10) ||
			!decodeCorroborationObject(request, &arguments) || arguments.OperationID != operation ||
			!decodeCorroborationObject(result, &evidence) || observation.Review == nil ||
			observation.Review.ID != evidence.ReviewID || observation.Review.NodeID != evidence.NodeID ||
			observation.Review.HeadSHA != evidence.CommitID || observation.Review.ActorID != evidence.ActorID ||
			observation.AuthorizedReviewerActorID != evidence.ActorID || expectedSHA != evidence.CommitID ||
			observation.Review.ChangeProposalID != proposal.PullRequestID ||
			(observation.Outcome == workflow.TurnOutcomeApproved && (arguments.Event != "APPROVE" || evidence.State != "APPROVED")) ||
			(observation.Outcome == workflow.TurnOutcomeChangesRequested && (arguments.Event != "REQUEST_CHANGES" || evidence.State != "CHANGES_REQUESTED")) ||
			(observation.Outcome != workflow.TurnOutcomeApproved && observation.Outcome != workflow.TurnOutcomeChangesRequested) {
			return ErrTerminalCorroborationConflict
		}
	default:
		return ErrTerminalCorroborationConflict
	}
	return nil
}

func decodeCorroborationObject(raw []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(&struct{}{}) == io.EOF
}
