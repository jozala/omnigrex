package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	// ErrAgentTurnSettlementInvalid means the structured observation cannot represent a safe settlement.
	ErrAgentTurnSettlementInvalid = errors.New("agent turn settlement observation is invalid")
	// ErrAgentTurnSettlementConflict means a settled turn and epoch was replayed with different input.
	ErrAgentTurnSettlementConflict = errors.New("agent turn settlement observation conflicts with durable settlement")
	// ErrAgentTurnSettlementRejected means the reducer refused to apply the observed settlement.
	ErrAgentTurnSettlementRejected = errors.New("agent turn settlement was not applied")
	// ErrAgentTurnChangeProposalConflict means the observed Pull Request conflicts with durable identity.
	ErrAgentTurnChangeProposalConflict = errors.New("agent turn Change Proposal identity conflict")
)

const maxAgentTurnDiagnosticRunes = 4096

const recoverySettlementDiagnostic = "Agent Turn execution was interrupted during recovery"

// AgentTurnSettlementAuthority identifies the fence which authorized settlement.
type AgentTurnSettlementAuthority string

const (
	AgentTurnSettlementLive     AgentTurnSettlementAuthority = "LIVE"
	AgentTurnSettlementRecovery AgentTurnSettlementAuthority = "RECOVERY"
)

// AgentTurnSettlementChangeProposal is the current Pull Request identity observed at settlement.
type AgentTurnSettlementChangeProposal struct {
	WorkflowID        string `json:"workflow_id"`
	RepositoryID      int64  `json:"repository_id"`
	RepositoryOwner   string `json:"repository_owner"`
	RepositoryName    string `json:"repository_name"`
	PullRequestID     int64  `json:"pull_request_id"`
	PullRequestNumber int64  `json:"pull_request_number"`
	PullRequestNodeID string `json:"pull_request_node_id"`
	Status            string `json:"status"`
	Active            bool   `json:"active"`
	BaseRef           string `json:"base_ref"`
	BaseSHA           string `json:"base_sha"`
	HeadRef           string `json:"head_ref"`
	HeadSHA           string `json:"head_sha"`
}

// AgentTurnSettlementObservation is structured orchestration evidence. ACP response text is not accepted here.
type AgentTurnSettlementObservation struct {
	ObservedAt                time.Time                          `json:"observed_at"`
	Outcome                   workflow.TurnOutcome               `json:"outcome"`
	ChangeProposal            *AgentTurnSettlementChangeProposal `json:"change_proposal,omitempty"`
	Review                    *workflow.ReviewIdentity           `json:"review,omitempty"`
	ExistingReview            *workflow.ReviewIdentity           `json:"existing_review,omitempty"`
	AuthorizedReviewerActorID int64                              `json:"authorized_reviewer_actor_id,omitempty"`
	Diagnostic                string                             `json:"diagnostic,omitempty"`
	Completion                AgentTurnCompletion                `json:"-"`
}

// AgentTurnSettlement is the durable result of atomically applying and finalizing one turn epoch.
type AgentTurnSettlement struct {
	ID                    string                       `json:"id"`
	WorkflowID            string                       `json:"workflow_id"`
	AgentTurnID           string                       `json:"agent_turn_id"`
	ExecutionEpoch        int64                        `json:"execution_epoch"`
	Authority             AgentTurnSettlementAuthority `json:"authority"`
	RecoveryJobID         string                       `json:"recovery_job_id,omitempty"`
	RecoveryJobKind       string                       `json:"recovery_job_kind,omitempty"`
	Disposition           workflow.Disposition         `json:"disposition"`
	Reason                workflow.Reason              `json:"reason"`
	State                 workflow.State               `json:"state"`
	Revision              uint64                       `json:"revision"`
	TerminalStatus        AgentTurnStatus              `json:"terminal_status"`
	ChangeProposalID      string                       `json:"change_proposal_id,omitempty"`
	PendingEventCount     uint32                       `json:"pending_event_count"`
	LatestObservedHeadSHA string                       `json:"latest_observed_head_sha,omitempty"`
	SuccessorJobID        string                       `json:"successor_job_id,omitempty"`
	ReconciliationJobID   string                       `json:"reconciliation_job_id,omitempty"`
	SettledAt             time.Time                    `json:"settled_at"`
}

type canonicalSettlementObservation struct {
	ObservedAt                time.Time                          `json:"observed_at"`
	Outcome                   workflow.TurnOutcome               `json:"outcome"`
	ChangeProposal            *AgentTurnSettlementChangeProposal `json:"change_proposal,omitempty"`
	Review                    *workflow.ReviewIdentity           `json:"review,omitempty"`
	ExistingReview            *workflow.ReviewIdentity           `json:"existing_review,omitempty"`
	AuthorizedReviewerActorID int64                              `json:"authorized_reviewer_actor_id,omitempty"`
	Diagnostic                string                             `json:"diagnostic,omitempty"`
	TerminalStatus            AgentTurnStatus                    `json:"terminal_status"`
	TerminalOutcome           json.RawMessage                    `json:"terminal_outcome,omitempty"`
	TerminalLastError         string                             `json:"terminal_last_error,omitempty"`
}

type persistedAgentTurnSettlement struct {
	result                                                       AgentTurnSettlement
	workflowAttemptID, assignmentID, sessionID, executionJobID   string
	jobLeaseOwner, jobLeaseToken, ownerID                        string
	executionEpoch, controlRevision                              int64
	jobAttempt                                                   int
	ownerTokenSHA256, observationSHA256, observation, resultJSON []byte
}

// SettleAgentTurn atomically reduces a structured observation, persists its provenance and actions,
// and finalizes the exact live Agent Turn execution fence.
func (store *Store) SettleAgentTurn(ctx context.Context, lease AgentTurnLease, observation AgentTurnSettlementObservation) (AgentTurnSettlement, error) {
	canonicalObservation, observationJSON, observationHash, err := canonicalizeAgentTurnSettlementObservation(observation)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	if !validSettlementLease(lease) {
		return AgentTurnSettlement{}, ErrAgentTurnFenceLost
	}
	settlementID, err := randomUUID()
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("settle Agent Turn: generate settlement identity: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("begin Agent Turn settlement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fmt.Sprintf("agent-turn-settlement:%s:%d", lease.ID, lease.ExecutionEpoch)); err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("serialize Agent Turn settlement: %w", err)
	}
	prior, err := readPersistedAgentTurnSettlement(ctx, tx, lease.ID, lease.ExecutionEpoch)
	if err == nil {
		if err := verifyAgentTurnSettlementReplay(ctx, tx, prior, lease, observationJSON, observationHash); err != nil {
			return AgentTurnSettlement{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnSettlement{}, fmt.Errorf("commit Agent Turn settlement replay: %w", err)
		}
		return prior.result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnSettlement{}, fmt.Errorf("read prior Agent Turn settlement: %w", err)
	}

	job, turn, role, snapshot, err := lockLiveAgentTurnSettlement(ctx, tx, lease)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	pending, err := derivePendingEventsObservation(ctx, tx, job.WorkflowID, turn.ID)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	proposalID, proposal, err := validateSettlementChangeProposal(ctx, tx, job, turn, role, snapshot, canonicalObservation)
	if err != nil {
		return AgentTurnSettlement{}, err
	}
	if err := validateSettlementReviewerActor(ctx, tx, job, role, canonicalObservation); err != nil {
		return AgentTurnSettlement{}, err
	}

	event := workflow.TurnSettledEvent{
		EventMetadata: workflow.EventMetadata{
			ID: settlementID, ObservedAt: canonicalObservation.ObservedAt,
			WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
		},
		Turn: workflow.TurnGuard{
			TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
			Role: role, Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
			ChangeProposalID: snapshot.ActiveTurn.ChangeProposalID,
			ExpectedHeadSHA:  turn.ExpectedHeadSHA,
		},
		Outcome: canonicalObservation.Outcome, ChangeProposal: proposal,
		Review: canonicalObservation.Review, ExistingReview: canonicalObservation.ExistingReview,
		AuthorizedReviewerActorID: canonicalObservation.AuthorizedReviewerActorID,
		PendingEvents:             pending, Diagnostic: canonicalObservation.Diagnostic,
	}
	decision := workflow.Reduce(snapshot, event)
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("validate Agent Turn settlement decision: %w", err)
	}
	if decision.Disposition != workflow.DispositionApplied {
		return AgentTurnSettlement{}, fmt.Errorf("%w: disposition %s, reason %s", ErrAgentTurnSettlementRejected, decision.Disposition, decision.Reason)
	}

	if snapshot.ChangeProposal == nil && canonicalObservation.ChangeProposal != nil {
		proposalID, err = insertInitialSettlementChangeProposal(ctx, tx, settlementID, job, turn, *canonicalObservation.ChangeProposal)
		if err != nil {
			return AgentTurnSettlement{}, err
		}
	}
	ownerHash := sha256.Sum256([]byte(lease.OwnerToken))
	_, err = tx.Exec(ctx, `
INSERT INTO agent_turn_settlements (
    id, workflow_id, workflow_attempt_id, agent_assignment_id, agent_session_id,
    agent_turn_id, execution_epoch, control_revision, authority, execution_job_id,
    job_attempt_number, job_lease_owner, job_lease_token, owner_id, owner_token_sha256,
    observation, observation_sha256, workflow_outcome, terminal_turn_status,
    terminal_turn_outcome, terminal_last_error, pending_event_count,
    latest_observed_head_sha, disposition, reason, workflow_state,
    workflow_revision, change_proposal_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'LIVE', $9, $10, $11, $12, $13, $14,
        $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`,
		settlementID, job.WorkflowID, job.WorkflowAttemptID, job.AgentAssignmentID,
		job.AgentSessionID, turn.ID, turn.ExecutionEpoch, turn.ControlRevision, job.ID,
		job.AttemptCount, job.LeaseOwner, job.LeaseToken, lease.OwnerID, ownerHash[:],
		observationJSON, observationHash[:], canonicalObservation.Outcome,
		canonicalObservation.TerminalStatus, nullableJSON(canonicalObservation.TerminalOutcome),
		nullableString(canonicalObservation.TerminalLastError), int64(pending.Count),
		nullableString(pending.LatestObservedHeadSHA), decision.Disposition, decision.Reason,
		decision.Snapshot.State, int64(decision.Snapshot.Revision), nullableString(proposalID))
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("persist Agent Turn settlement provenance: %w", err)
	}
	if err := persistAppliedSettlementDecision(ctx, tx, settlementID, job.WorkflowID, snapshot, decision); err != nil {
		return AgentTurnSettlement{}, err
	}

	result := AgentTurnSettlement{
		ID: settlementID, WorkflowID: job.WorkflowID, AgentTurnID: turn.ID,
		ExecutionEpoch: turn.ExecutionEpoch, Authority: AgentTurnSettlementLive, Disposition: decision.Disposition,
		Reason: decision.Reason, State: decision.Snapshot.State, Revision: decision.Snapshot.Revision,
		TerminalStatus: canonicalObservation.TerminalStatus, ChangeProposalID: proposalID,
		PendingEventCount: pending.Count, LatestObservedHeadSHA: pending.LatestObservedHeadSHA,
	}
	if err := resolveSettlementActionJobs(ctx, tx, settlementID, &result); err != nil {
		return AgentTurnSettlement{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&result.SettledAt); err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("timestamp Agent Turn settlement: %w", err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("encode Agent Turn settlement result: %w", err)
	}
	if err := finalizeSettledAgentTurn(ctx, tx, lease, job, turn, canonicalObservation, settlementID); err != nil {
		return AgentTurnSettlement{}, err
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turn_settlements
SET successor_job_id = $2, reconciliation_job_id = $3, result = $4, settled_at = $5
WHERE id = $1 AND settled_at IS NULL`, settlementID, nullableString(result.SuccessorJobID),
		nullableString(result.ReconciliationJobID), resultJSON, result.SettledAt)
	if err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("complete Agent Turn settlement provenance: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return AgentTurnSettlement{}, ErrAgentTurnSettlementConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnSettlement{}, fmt.Errorf("commit Agent Turn settlement: %w", err)
	}
	return result, nil
}

// settleAgentTurnRecoveryTx applies the one recovery-owned infrastructure transition when
// the caller's exact recovery job acknowledgement completes the final barrier.
func settleAgentTurnRecoveryTx(ctx context.Context, tx pgx.Tx, authority Job) (bool, error) {
	if authority.Kind != StopStaleRuntimeJobKind && authority.Kind != ReconcileAgentTurnMutationsJobKind {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	turn, err := lockAgentTurn(ctx, tx, authority.AgentTurnID)
	if err != nil {
		return false, err
	}
	var stopJobID, reconcileJobID, continuation, originalOwnerID string
	var recoveryStartedAt time.Time
	var recoverySettled bool
	var originalOwnerHash []byte
	var stopStatus JobStatus
	reconcileStatus := JobSucceeded
	if err := tx.QueryRow(ctx, `
SELECT turn.stop_runtime_job_id::text, COALESCE(turn.reconcile_mutations_job_id::text, ''),
       turn.recovery_continuation, turn.recovery_started_at,
       turn.recovery_settled_at IS NOT NULL, COALESCE(turn.recovery_original_owner_id, ''),
       COALESCE(turn.recovery_original_owner_token_sha256, ''::bytea),
       (SELECT status FROM jobs WHERE id = turn.stop_runtime_job_id),
       COALESCE((SELECT status FROM jobs WHERE id = turn.reconcile_mutations_job_id), 'SUCCEEDED')
FROM agent_turns AS turn
WHERE turn.id = $1 AND turn.execution_epoch = $2
  AND turn.recovery_started_at IS NOT NULL`, authority.AgentTurnID, authority.ExecutionEpoch).Scan(
		&stopJobID, &reconcileJobID, &continuation, &recoveryStartedAt, &recoverySettled,
		&originalOwnerID, &originalOwnerHash, &stopStatus, &reconcileStatus,
	); err != nil {
		return false, fmt.Errorf("inspect Agent Turn recovery settlement: %w", err)
	}
	if recoverySettled {
		return false, nil
	}
	registeredAuthority := authority.Kind == StopStaleRuntimeJobKind && authority.ID == stopJobID ||
		authority.Kind == ReconcileAgentTurnMutationsJobKind && authority.ID == reconcileJobID
	if !registeredAuthority || authority.Status != JobLeased || authority.AttemptCount <= 0 ||
		authority.LeaseOwner == "" || authority.LeaseToken == "" {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	var authorityAttemptOwner, authorityAttemptToken, authorityAttemptStatus string
	if err := tx.QueryRow(ctx, `
SELECT lease_owner, lease_token::text, status
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2`,
		authority.ID, authority.AttemptCount).Scan(
		&authorityAttemptOwner, &authorityAttemptToken, &authorityAttemptStatus,
	); err != nil || authorityAttemptOwner != authority.LeaseOwner ||
		authorityAttemptToken != authority.LeaseToken || authorityAttemptStatus != string(JobSucceeded) {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, turn.ID, turn.ExecutionEpoch).Scan(&unsettled); err != nil {
		return false, fmt.Errorf("inspect recovered mutations for settlement: %w", err)
	}
	var runtimeStopped bool
	if err := tx.QueryRow(ctx, `SELECT runtime_stopped_at IS NOT NULL FROM agent_turns WHERE id = $1`, turn.ID).Scan(&runtimeStopped); err != nil {
		return false, fmt.Errorf("inspect recovered Runtime Process settlement: %w", err)
	}
	if !runtimeStopped || stopStatus != JobSucceeded || reconcileStatus != JobSucceeded || unsettled {
		return false, nil
	}

	if continuation == recoveryContinuationMutationHandoff || continuation == recoveryContinuationMigrationHandoff {
		result, err := tx.Exec(ctx, `
UPDATE agent_turns
SET status = 'INTERRUPTED', recovery_settled_at = clock_timestamp()
WHERE id = $1 AND execution_epoch = $2 AND recovery_settled_at IS NULL
  AND recovery_continuation = $3`, turn.ID, turn.ExecutionEpoch, continuation)
		if err != nil {
			return false, fmt.Errorf("settle Human Handoff Agent Turn recovery: %w", err)
		}
		return result.RowsAffected() == 1, nil
	}
	if continuation != recoveryContinuationPendingInfrastructure || originalOwnerID == "" || len(originalOwnerHash) != sha256.Size {
		return false, ErrAgentTurnRecoveryFenceLost
	}

	var identity recoveryJobIdentity
	if err := json.Unmarshal(authority.Payload, &identity); err != nil ||
		identity.ExecutionJobID == "" || identity.OwnerID != originalOwnerID ||
		identity.OwnerTokenSHA256 != fmt.Sprintf("%x", originalOwnerHash) {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	var executionAttempt int
	var executionLeaseOwner, executionLeaseToken string
	if err := tx.QueryRow(ctx, `
SELECT execution.attempt_count, attempt.lease_owner, attempt.lease_token::text
FROM jobs AS execution
JOIN job_attempts AS attempt ON attempt.job_id = execution.id
    AND attempt.attempt_number = execution.attempt_count
WHERE execution.id = $1 AND execution.kind = 'RUN_AGENT_TURN'
  AND execution.workflow_id = $2 AND execution.workflow_attempt_id = $3
  AND execution.agent_assignment_id = $4 AND execution.agent_session_id = $5
  AND execution.agent_turn_id = $6 AND execution.execution_epoch = $7
  AND execution.status IN ('SUCCEEDED', 'FAILED')`, identity.ExecutionJobID,
		authority.WorkflowID, authority.WorkflowAttemptID, authority.AgentAssignmentID,
		authority.AgentSessionID, authority.AgentTurnID, authority.ExecutionEpoch).Scan(
		&executionAttempt, &executionLeaseOwner, &executionLeaseToken,
	); err != nil {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	var role workflow.Role
	if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments WHERE id = $1 AND workflow_id = $2`,
		authority.AgentAssignmentID, authority.WorkflowID).Scan(&role); err != nil {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	var changeProposalID int64
	if turn.ChangeProposalID != "" {
		if err := tx.QueryRow(ctx, `SELECT pull_request_id FROM change_proposals WHERE id = $1 AND workflow_id = $2`,
			turn.ChangeProposalID, authority.WorkflowID).Scan(&changeProposalID); err != nil {
			return false, ErrAgentTurnRecoveryFenceLost
		}
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, authority.WorkflowID)
	if err != nil {
		return false, fmt.Errorf("rehydrate recovered Agent Turn Workflow: %w", err)
	}
	if snapshot.ActiveTurn != nil || snapshot.CurrentAttempt == nil || snapshot.CurrentAttempt.ID != turn.WorkflowAttemptID {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	guard := workflow.TurnGuard{
		TurnID: turn.ID, SessionID: turn.AgentSessionID, AttemptID: turn.WorkflowAttemptID,
		Role: role, Epoch: uint64(turn.ExecutionEpoch), ControlRevision: uint64(turn.ControlRevision),
		ChangeProposalID: changeProposalID, ExpectedHeadSHA: turn.ExpectedHeadSHA,
	}
	snapshot.ActiveTurn = &workflow.ActiveTurn{
		ID: guard.TurnID, SessionID: guard.SessionID, AttemptID: guard.AttemptID,
		Role: guard.Role, Epoch: guard.Epoch, ControlRevision: guard.ControlRevision,
		ChangeProposalID: guard.ChangeProposalID, ExpectedHeadSHA: guard.ExpectedHeadSHA,
	}
	pending, err := derivePendingEventsObservation(ctx, tx, authority.WorkflowID, turn.ID)
	if err != nil {
		return false, err
	}
	observation, observationJSON, observationHash, err := canonicalizeAgentTurnSettlementObservation(AgentTurnSettlementObservation{
		ObservedAt: recoveryStartedAt, Outcome: workflow.TurnOutcomeInfrastructureFailed,
		Diagnostic: recoverySettlementDiagnostic,
		Completion: AgentTurnCompletion{Status: AgentTurnInterrupted, LastError: recoverySettlementDiagnostic},
	})
	if err != nil {
		return false, err
	}
	settlementID, err := randomUUID()
	if err != nil {
		return false, fmt.Errorf("generate recovered Agent Turn settlement identity: %w", err)
	}
	decision := workflow.Reduce(snapshot, workflow.TurnSettledEvent{
		EventMetadata: workflow.EventMetadata{
			ID: settlementID, ObservedAt: observation.ObservedAt,
			WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision,
		},
		Turn: guard, Outcome: workflow.TurnOutcomeInfrastructureFailed,
		PendingEvents: pending, Diagnostic: observation.Diagnostic,
	})
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return false, fmt.Errorf("validate recovered Agent Turn settlement decision: %w", err)
	}
	if decision.Disposition != workflow.DispositionApplied {
		return false, fmt.Errorf("%w: disposition %s, reason %s", ErrAgentTurnSettlementRejected, decision.Disposition, decision.Reason)
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
        $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, NULL, $24, $25,
        $26, $27, $28, $29, $30, $31)`, settlementID, authority.WorkflowID,
		authority.WorkflowAttemptID, authority.AgentAssignmentID, authority.AgentSessionID,
		turn.ID, turn.ExecutionEpoch, turn.ControlRevision, identity.ExecutionJobID,
		executionAttempt, executionLeaseOwner, executionLeaseToken, originalOwnerID,
		originalOwnerHash, authority.ID, authority.Kind, authority.AttemptCount,
		authority.LeaseOwner, authority.LeaseToken, observationJSON, observationHash[:],
		workflow.TurnOutcomeInfrastructureFailed, AgentTurnInterrupted,
		recoverySettlementDiagnostic, int64(pending.Count), nullableString(pending.LatestObservedHeadSHA),
		decision.Disposition, decision.Reason, decision.Snapshot.State,
		int64(decision.Snapshot.Revision), nullableString(turn.ChangeProposalID))
	if err != nil {
		return false, fmt.Errorf("persist recovered Agent Turn settlement provenance: %w", err)
	}
	if err := persistAppliedSettlementDecision(ctx, tx, settlementID, authority.WorkflowID, snapshot, decision); err != nil {
		return false, err
	}
	result := AgentTurnSettlement{
		ID: settlementID, WorkflowID: authority.WorkflowID, AgentTurnID: turn.ID,
		ExecutionEpoch: turn.ExecutionEpoch, Authority: AgentTurnSettlementRecovery,
		RecoveryJobID: authority.ID, RecoveryJobKind: authority.Kind,
		Disposition: decision.Disposition, Reason: decision.Reason,
		State: decision.Snapshot.State, Revision: decision.Snapshot.Revision,
		TerminalStatus: AgentTurnInterrupted, ChangeProposalID: turn.ChangeProposalID,
		PendingEventCount: pending.Count, LatestObservedHeadSHA: pending.LatestObservedHeadSHA,
	}
	if err := resolveSettlementActionJobs(ctx, tx, settlementID, &result); err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&result.SettledAt); err != nil {
		return false, fmt.Errorf("timestamp recovered Agent Turn settlement: %w", err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("encode recovered Agent Turn settlement result: %w", err)
	}
	turnResult, err := tx.Exec(ctx, `
UPDATE agent_turns AS turn
SET status = 'INTERRUPTED', outcome = NULL, last_error = $3,
    completed_at = COALESCE(completed_at, $4), recovery_settled_at = $4,
    recovery_continuation = 'INFRASTRUCTURE_FAILURE_APPLIED', recovery_settlement_id = $5
WHERE turn.id = $1 AND turn.execution_epoch = $2
  AND turn.recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE'
  AND turn.recovery_settled_at IS NULL AND turn.runtime_stopped_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM jobs WHERE id = turn.stop_runtime_job_id AND status = 'SUCCEEDED')
  AND (turn.reconcile_mutations_job_id IS NULL OR EXISTS (
      SELECT 1 FROM jobs WHERE id = turn.reconcile_mutations_job_id AND status = 'SUCCEEDED'
  ))
  AND NOT EXISTS (
      SELECT 1 FROM tool_invocations
      WHERE agent_turn_id = turn.id AND execution_epoch = turn.execution_epoch
        AND kind = 'MUTATION' AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
  )`, turn.ID, turn.ExecutionEpoch, recoverySettlementDiagnostic, result.SettledAt, settlementID)
	if err != nil || turnResult.RowsAffected() != 1 {
		return false, ErrAgentTurnRecoveryFenceLost
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turn_settlements
SET successor_job_id = $2, reconciliation_job_id = $3, result = $4, settled_at = $5
WHERE id = $1 AND settled_at IS NULL`, settlementID, nullableString(result.SuccessorJobID),
		nullableString(result.ReconciliationJobID), resultJSON, result.SettledAt)
	if err != nil {
		return false, fmt.Errorf("complete recovered Agent Turn settlement provenance: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return false, ErrAgentTurnSettlementConflict
	}
	return true, nil
}

func canonicalizeAgentTurnSettlementObservation(observation AgentTurnSettlementObservation) (canonicalSettlementObservation, json.RawMessage, [sha256.Size]byte, error) {
	var result canonicalSettlementObservation
	result.ObservedAt = observation.ObservedAt.UTC()
	result.Outcome = observation.Outcome
	result.ChangeProposal = observation.ChangeProposal
	result.Review = observation.Review
	result.ExistingReview = observation.ExistingReview
	result.AuthorizedReviewerActorID = observation.AuthorizedReviewerActorID
	result.Diagnostic = sanitizeAgentTurnDiagnostic(observation.Diagnostic)
	result.TerminalStatus = observation.Completion.Status
	result.TerminalLastError = sanitizeAgentTurnDiagnostic(observation.Completion.LastError)
	if len(observation.Completion.Outcome) != 0 {
		outcome, err := canonicalJSON(observation.Completion.Outcome)
		if err != nil {
			return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: terminal outcome: %v", ErrAgentTurnSettlementInvalid, err)
		}
		result.TerminalOutcome = outcome
	}
	if result.ObservedAt.IsZero() {
		return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: observed time is required", ErrAgentTurnSettlementInvalid)
	}
	successful := result.Outcome == workflow.TurnOutcomeChangeProposalReady ||
		result.Outcome == workflow.TurnOutcomeChangesRequested || result.Outcome == workflow.TurnOutcomeApproved ||
		result.Outcome == workflow.TurnOutcomeBlocked
	if successful && (result.TerminalStatus != AgentTurnSucceeded || result.TerminalLastError != "") {
		return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: successful Workflow outcome requires a successful terminal Turn", ErrAgentTurnSettlementInvalid)
	}
	if result.Outcome == workflow.TurnOutcomeInfrastructureFailed &&
		(result.TerminalStatus != AgentTurnFailed && result.TerminalStatus != AgentTurnInterrupted && result.TerminalStatus != AgentTurnTimedOut || result.TerminalLastError == "") {
		return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: infrastructure failure requires a failed terminal Turn and diagnostic", ErrAgentTurnSettlementInvalid)
	}
	if !successful && result.Outcome != workflow.TurnOutcomeInfrastructureFailed {
		return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: unsupported Workflow outcome", ErrAgentTurnSettlementInvalid)
	}
	switch result.Outcome {
	case workflow.TurnOutcomeChangeProposalReady:
		if result.ChangeProposal == nil || result.Review != nil || result.ExistingReview != nil || result.AuthorizedReviewerActorID != 0 {
			return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: Developer outcome shape", ErrAgentTurnSettlementInvalid)
		}
	case workflow.TurnOutcomeChangesRequested, workflow.TurnOutcomeApproved:
		if result.ChangeProposal == nil || result.Review == nil || result.AuthorizedReviewerActorID <= 0 {
			return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: Reviewer outcome shape", ErrAgentTurnSettlementInvalid)
		}
	case workflow.TurnOutcomeBlocked, workflow.TurnOutcomeInfrastructureFailed:
		if result.ChangeProposal != nil || result.Review != nil || result.ExistingReview != nil || result.AuthorizedReviewerActorID != 0 {
			return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: terminal outcome has unrelated review evidence", ErrAgentTurnSettlementInvalid)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return canonicalSettlementObservation{}, nil, [sha256.Size]byte{}, fmt.Errorf("%w: encode observation: %v", ErrAgentTurnSettlementInvalid, err)
	}
	hash := sha256.Sum256(encoded)
	return result, encoded, hash, nil
}

func sanitizeAgentTurnDiagnostic(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxAgentTurnDiagnosticRunes {
		value = string(runes[:maxAgentTurnDiagnosticRunes])
	}
	return value
}

func validSettlementLease(lease AgentTurnLease) bool {
	return validUUID(lease.ID) && validUUID(lease.AgentAssignmentID) && validUUID(lease.AgentSessionID) &&
		validUUID(lease.JobLease.ID) && validUUID(lease.JobLease.LeaseToken) && validUUID(lease.OwnerToken) &&
		lease.ExecutionEpoch > 0 && lease.ControlRevision > 0 && lease.JobLease.Attempt > 0 &&
		strings.TrimSpace(lease.JobLease.LeaseOwner) != "" && strings.TrimSpace(lease.OwnerID) != ""
}

func lockLiveAgentTurnSettlement(ctx context.Context, tx pgx.Tx, lease AgentTurnLease) (Job, lockedTurn, workflow.Role, workflow.Snapshot, error) {
	job, err := lockAgentTurnJob(ctx, tx, lease.JobLease, true)
	if err != nil || job.LeaseOwner != lease.JobLease.LeaseOwner || !sameAgentTurnExecutionIdentity(job, lease) {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnFenceLost
	}
	hierarchy, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false)
	if err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, lease.ID)
	if err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, err
	}
	turn.AgentAssignmentID = job.AgentAssignmentID
	if !hierarchy.allowsActiveTurn() || hierarchy.controlRevision != lease.ControlRevision ||
		!turn.active || turn.Status != AgentTurnSettling || turn.MutationAdmissionOpen ||
		turn.AgentAssignmentID != lease.AgentAssignmentID || turn.AgentSessionID != lease.AgentSessionID ||
		turn.WorkflowAttemptID != lease.WorkflowAttemptID || turn.ExecutionEpoch != lease.ExecutionEpoch ||
		turn.ControlRevision != lease.ControlRevision || turn.ownerID != lease.OwnerID ||
		turn.ownerToken != lease.OwnerToken || !turn.leaseLive {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnFenceLost
	}
	var admissionClosed, unsettled bool
	if err := tx.QueryRow(ctx, `
SELECT mutation_admission_closed_at IS NOT NULL,
       EXISTS (SELECT 1 FROM tool_invocations WHERE agent_turn_id = $1
           AND execution_epoch = $2 AND kind = 'MUTATION'
           AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING'))
FROM agent_turns WHERE id = $1`, turn.ID, turn.ExecutionEpoch).Scan(&admissionClosed, &unsettled); err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, fmt.Errorf("inspect Agent Turn settlement barrier: %w", err)
	}
	if !admissionClosed {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrMutationAdmissionClosed
	}
	if unsettled {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnMutationsUnsettled
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, err
	}
	var slotLive bool
	err = tx.QueryRow(ctx, `
SELECT lease_expires_at > clock_timestamp() FROM agent_turn_slots
WHERE agent_turn_id = $1 AND agent_session_id = $2 AND execution_epoch = $3
  AND control_revision = $4 AND owner_id = $5 AND owner_token = $6 FOR UPDATE`,
		turn.ID, turn.AgentSessionID, turn.ExecutionEpoch, turn.ControlRevision,
		lease.OwnerID, lease.OwnerToken).Scan(&slotLive)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !slotLive {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, fmt.Errorf("lock Agent Turn settlement slot: %w", err)
	}
	var role workflow.Role
	if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnFenceLost
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, fmt.Errorf("rehydrate active Agent Turn Workflow: %w", err)
	}
	if snapshot.ActiveTurn == nil || snapshot.ActiveTurn.ID != turn.ID {
		return Job{}, lockedTurn{}, "", workflow.Snapshot{}, ErrAgentTurnFenceLost
	}
	return job, turn, role, snapshot, nil
}

func derivePendingEventsObservation(ctx context.Context, tx pgx.Tx, workflowID, turnID string) (workflow.PendingEventsObservation, error) {
	rows, err := tx.Query(ctx, `
SELECT event.delivery_id::text, event.payload, delivery.event_name, delivery.action,
       COALESCE(delivery.repository_id, 0), COALESCE(delivery.repository_owner, ''),
       COALESCE(delivery.repository_name, '')
FROM normalized_events AS event
JOIN webhook_deliveries AS delivery USING (delivery_id)
WHERE event.workflow_id = $1 AND event.deferred_for_turn_id = $2
  AND event.status = 'DEFERRED'
ORDER BY event.created_at, event.delivery_id
FOR UPDATE OF event, delivery`, workflowID, turnID)
	if err != nil {
		return workflow.PendingEventsObservation{}, fmt.Errorf("lock deferred Agent Turn events: %w", err)
	}
	defer rows.Close()
	var pending workflow.PendingEventsObservation
	for rows.Next() {
		var deliveryID, eventName, action, repositoryOwner, repositoryName string
		var repositoryID int64
		var payload []byte
		if err := rows.Scan(&deliveryID, &payload, &eventName, &action, &repositoryID, &repositoryOwner, &repositoryName); err != nil {
			return workflow.PendingEventsObservation{}, fmt.Errorf("scan deferred Agent Turn event: %w", err)
		}
		pending.Count++
		var normalized struct {
			DeliveryID string `json:"delivery_id"`
			EventName  string `json:"event"`
			Action     string `json:"action"`
			Repository struct {
				ID    int64  `json:"id"`
				Owner string `json:"owner"`
				Name  string `json:"name"`
			} `json:"repository"`
			PullRequest *struct {
				ID        int64  `json:"id"`
				Number    int64  `json:"number"`
				BaseRef   string `json:"base_ref"`
				BaseSHA   string `json:"base_sha"`
				HeadRef   string `json:"head_ref"`
				HeadSHA   string `json:"head_sha"`
				BeforeSHA string `json:"before_sha"`
			} `json:"pull_request"`
		}
		if json.Unmarshal(payload, &normalized) != nil {
			continue
		}
		pullRequest := normalized.PullRequest
		validPullRequest := pullRequest != nil && pullRequest.ID > 0 && pullRequest.Number > 0 &&
			strings.TrimSpace(pullRequest.BaseRef) != "" && strings.TrimSpace(pullRequest.BaseSHA) != "" &&
			strings.TrimSpace(pullRequest.HeadRef) != "" && strings.TrimSpace(pullRequest.HeadSHA) != "" &&
			strings.TrimSpace(pullRequest.BeforeSHA) != ""
		if normalized.DeliveryID == deliveryID &&
			normalized.EventName == "pull_request" && normalized.EventName == eventName &&
			normalized.Action == "synchronize" && normalized.Action == action &&
			normalized.Repository.ID > 0 && normalized.Repository.ID == repositoryID &&
			strings.TrimSpace(normalized.Repository.Owner) != "" && normalized.Repository.Owner == repositoryOwner &&
			strings.TrimSpace(normalized.Repository.Name) != "" && normalized.Repository.Name == repositoryName &&
			validPullRequest {
			pending.LatestObservedHeadSHA = pullRequest.HeadSHA
		}
	}
	if err := rows.Err(); err != nil {
		return workflow.PendingEventsObservation{}, fmt.Errorf("read deferred Agent Turn events: %w", err)
	}
	pending.RequiresReconciliation = pending.LatestObservedHeadSHA != ""
	return pending, nil
}

func validateSettlementChangeProposal(ctx context.Context, tx pgx.Tx, job Job, turn lockedTurn, role workflow.Role, snapshot workflow.Snapshot, observation canonicalSettlementObservation) (string, *workflow.ChangeProposal, error) {
	if observation.ChangeProposal == nil {
		return turn.ChangeProposalID, nil, nil
	}
	observed := observation.ChangeProposal
	if observed.WorkflowID != job.WorkflowID || observed.RepositoryID <= 0 ||
		strings.TrimSpace(observed.RepositoryOwner) == "" || strings.TrimSpace(observed.RepositoryName) == "" ||
		observed.PullRequestID <= 0 || observed.PullRequestNumber <= 0 || strings.TrimSpace(observed.PullRequestNodeID) == "" ||
		observed.Status != "OPEN" || !observed.Active || strings.TrimSpace(observed.BaseRef) == "" ||
		strings.TrimSpace(observed.BaseSHA) == "" || strings.TrimSpace(observed.HeadRef) == "" || strings.TrimSpace(observed.HeadSHA) == "" {
		return "", nil, fmt.Errorf("%w: incomplete observed Pull Request", ErrAgentTurnChangeProposalConflict)
	}
	var repositoryID int64
	var owner, name string
	if err := tx.QueryRow(ctx, `SELECT repository_id, repository_owner, repository_name FROM workflows WHERE id = $1`, job.WorkflowID).Scan(&repositoryID, &owner, &name); err != nil {
		return "", nil, fmt.Errorf("read settlement repository identity: %w", err)
	}
	if observed.RepositoryID != repositoryID || observed.RepositoryOwner != owner || observed.RepositoryName != name {
		return "", nil, ErrAgentTurnChangeProposalConflict
	}
	proposal := &workflow.ChangeProposal{ID: observed.PullRequestID, Number: observed.PullRequestNumber, HeadSHA: observed.HeadSHA, Open: true}
	if snapshot.ChangeProposal == nil {
		if role != workflow.RoleDeveloper || observation.Outcome != workflow.TurnOutcomeChangeProposalReady || turn.ChangeProposalID != "" {
			return "", nil, ErrAgentTurnChangeProposalConflict
		}
		return "", proposal, nil
	}
	if turn.ChangeProposalID == "" || snapshot.ChangeProposal.ID != observed.PullRequestID {
		return "", nil, ErrAgentTurnChangeProposalConflict
	}
	var proposalID, workflowID, repositoryOwner, repositoryName, nodeID, status, baseRef, headRef string
	var storedRepositoryID, pullRequestID, pullRequestNumber int64
	var active bool
	err := tx.QueryRow(ctx, `
SELECT id::text, workflow_id::text, repository_id, repository_owner, repository_name,
       pull_request_id, pull_request_number, COALESCE(pull_request_node_id, ''), status,
       active, base_ref, head_ref
FROM change_proposals WHERE id = $1 FOR UPDATE`, turn.ChangeProposalID).Scan(
		&proposalID, &workflowID, &storedRepositoryID, &repositoryOwner, &repositoryName,
		&pullRequestID, &pullRequestNumber, &nodeID, &status, &active, &baseRef, &headRef)
	if err != nil || workflowID != observed.WorkflowID || storedRepositoryID != observed.RepositoryID ||
		repositoryOwner != observed.RepositoryOwner || repositoryName != observed.RepositoryName ||
		pullRequestID != observed.PullRequestID || pullRequestNumber != observed.PullRequestNumber ||
		nodeID != observed.PullRequestNodeID || status != observed.Status || active != observed.Active ||
		baseRef != observed.BaseRef || headRef != observed.HeadRef {
		return "", nil, ErrAgentTurnChangeProposalConflict
	}
	return proposalID, proposal, nil
}

func validateSettlementReviewerActor(ctx context.Context, tx pgx.Tx, job Job, role workflow.Role, observation canonicalSettlementObservation) error {
	if observation.Outcome != workflow.TurnOutcomeChangesRequested && observation.Outcome != workflow.TurnOutcomeApproved {
		return nil
	}
	if role != workflow.RoleReviewer || observation.Review == nil {
		return fmt.Errorf("%w: Reviewer outcome has no Reviewer turn", ErrAgentTurnSettlementInvalid)
	}
	var actorID int64
	err := tx.QueryRow(ctx, `
SELECT COALESCE(github_app_actor_id, 0) FROM agent_assignments
WHERE id = $1 AND workflow_id = $2 AND role = 'REVIEWER'
  AND status = 'ACTIVE' AND state_deleted_at IS NULL`, job.AgentAssignmentID, job.WorkflowID).Scan(&actorID)
	if err != nil || actorID <= 0 || actorID != observation.AuthorizedReviewerActorID || actorID != observation.Review.ActorID {
		return ErrReviewerActorConflict
	}
	return nil
}

func insertInitialSettlementChangeProposal(ctx context.Context, tx pgx.Tx, settlementID string, job Job, turn lockedTurn, proposal AgentTurnSettlementChangeProposal) (string, error) {
	id, err := randomUUID()
	if err != nil {
		return "", fmt.Errorf("generate initial Change Proposal identity: %w", err)
	}
	result, err := tx.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, created_by_turn_id, repository_id, repository_owner,
    repository_name, pull_request_id, pull_request_number, pull_request_node_id,
    status, active, base_ref, base_sha, head_ref, head_sha
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, TRUE, $11, $12, $13, $14
WHERE NOT EXISTS (SELECT 1 FROM change_proposals WHERE workflow_id = $2 AND active)`,
		id, job.WorkflowID, turn.ID, proposal.RepositoryID, proposal.RepositoryOwner,
		proposal.RepositoryName, proposal.PullRequestID, proposal.PullRequestNumber,
		proposal.PullRequestNodeID, proposal.Status, proposal.BaseRef, proposal.BaseSHA,
		proposal.HeadRef, proposal.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("%w: insert initial Pull Request for settlement %s: %v", ErrAgentTurnChangeProposalConflict, settlementID, err)
	}
	if result.RowsAffected() != 1 {
		return "", ErrAgentTurnChangeProposalConflict
	}
	return id, nil
}

func resolveSettlementActionJobs(ctx context.Context, tx pgx.Tx, settlementID string, result *AgentTurnSettlement) error {
	rows, err := tx.Query(ctx, `
SELECT id::text, kind FROM jobs WHERE agent_turn_settlement_id = $1
  AND kind IN ('PREPARE_AGENT_TURN', 'RECONCILE_PENDING_EVENTS')`, settlementID)
	if err != nil {
		return fmt.Errorf("read Agent Turn settlement actions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return fmt.Errorf("scan Agent Turn settlement action: %w", err)
		}
		switch kind {
		case PrepareAgentTurnJobKind:
			if result.SuccessorJobID != "" {
				return ErrWorkflowSuccessorConflict
			}
			result.SuccessorJobID = id
		case ReconcilePendingEventsJobKind:
			if result.ReconciliationJobID != "" {
				return ErrWorkflowSuccessorConflict
			}
			result.ReconciliationJobID = id
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read Agent Turn settlement actions: %w", err)
	}
	if result.SuccessorJobID != "" && result.ReconciliationJobID != "" || result.PendingEventCount > 0 && result.ReconciliationJobID == "" {
		return ErrWorkflowSuccessorConflict
	}
	return nil
}

func finalizeSettledAgentTurn(ctx context.Context, tx pgx.Tx, lease AgentTurnLease, job Job, turn lockedTurn, observation canonicalSettlementObservation, settlementID string) error {
	jobResult, err := json.Marshal(map[string]any{
		"agent_turn_settlement_id": settlementID,
		"turn_status":              observation.TerminalStatus,
		"workflow_outcome":         observation.Outcome,
	})
	if err != nil {
		return fmt.Errorf("encode settled Agent Turn job result: %w", err)
	}
	attemptUpdate, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_owner = $2 AND lease_token = $3
  AND attempt_number = $5 AND status = 'LEASED'`, job.ID, job.LeaseOwner, job.LeaseToken,
		jobResult, job.AttemptCount)
	if err != nil {
		return fmt.Errorf("complete settled Agent Turn job attempt: %w", err)
	}
	if attemptUpdate.RowsAffected() != 1 {
		return ErrAgentTurnFenceLost
	}
	jobUpdate, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    updated_at = clock_timestamp(), completed_at = clock_timestamp(),
    result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, jobResult)
	if err != nil {
		return fmt.Errorf("complete settled Agent Turn job: %w", err)
	}
	if jobUpdate.RowsAffected() != 1 {
		return ErrAgentTurnFenceLost
	}
	turnUpdate, err := tx.Exec(ctx, `
UPDATE agent_turns SET status = $5, active = FALSE, owner_id = NULL, owner_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), outcome = $6, last_error = $7
WHERE id = $1 AND execution_epoch = $2 AND control_revision = $3
  AND owner_token = $4 AND active AND status = 'SETTLING'
  AND NOT mutation_admission_open AND mutation_admission_closed_at IS NOT NULL`,
		turn.ID, turn.ExecutionEpoch, turn.ControlRevision, lease.OwnerToken,
		observation.TerminalStatus, nullableJSON(observation.TerminalOutcome),
		nullableString(observation.TerminalLastError))
	if err != nil {
		return fmt.Errorf("finalize settled Agent Turn: %w", err)
	}
	if turnUpdate.RowsAffected() != 1 {
		return ErrAgentTurnFenceLost
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return err
	}
	slotDelete, err := tx.Exec(ctx, `
DELETE FROM agent_turn_slots WHERE agent_turn_id = $1 AND agent_session_id = $2
  AND execution_epoch = $3 AND control_revision = $4 AND owner_id = $5 AND owner_token = $6`,
		turn.ID, turn.AgentSessionID, turn.ExecutionEpoch, turn.ControlRevision,
		lease.OwnerID, lease.OwnerToken)
	if err != nil {
		return fmt.Errorf("release settled Agent Turn slot: %w", err)
	}
	if slotDelete.RowsAffected() != 1 {
		return ErrAgentTurnFenceLost
	}
	return nil
}

func readPersistedAgentTurnSettlement(ctx context.Context, tx pgx.Tx, turnID string, epoch int64) (persistedAgentTurnSettlement, error) {
	var persisted persistedAgentTurnSettlement
	err := tx.QueryRow(ctx, `
SELECT workflow_id::text, workflow_attempt_id::text, agent_assignment_id::text,
       agent_session_id::text, execution_job_id::text, job_attempt_number,
       job_lease_owner, job_lease_token::text, owner_id, owner_token_sha256,
       execution_epoch, control_revision, authority,
       COALESCE(recovery_job_id::text, ''), COALESCE(recovery_job_kind, ''),
       observation, observation_sha256, result
FROM agent_turn_settlements
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND settled_at IS NOT NULL
FOR UPDATE`, turnID, epoch).Scan(
		&persisted.result.WorkflowID, &persisted.workflowAttemptID, &persisted.assignmentID,
		&persisted.sessionID, &persisted.executionJobID, &persisted.jobAttempt,
		&persisted.jobLeaseOwner, &persisted.jobLeaseToken, &persisted.ownerID,
		&persisted.ownerTokenSHA256, &persisted.executionEpoch, &persisted.controlRevision,
		&persisted.result.Authority, &persisted.result.RecoveryJobID, &persisted.result.RecoveryJobKind,
		&persisted.observation, &persisted.observationSHA256, &persisted.resultJSON)
	if err != nil {
		return persistedAgentTurnSettlement{}, err
	}
	if err := json.Unmarshal(persisted.resultJSON, &persisted.result); err != nil {
		return persistedAgentTurnSettlement{}, fmt.Errorf("decode durable Agent Turn settlement result: %w", err)
	}
	return persisted, nil
}

func verifyAgentTurnSettlementReplay(ctx context.Context, tx pgx.Tx, persisted persistedAgentTurnSettlement, lease AgentTurnLease, observation json.RawMessage, observationHash [sha256.Size]byte) error {
	ownerHash := sha256.Sum256([]byte(lease.OwnerToken))
	if persisted.result.Authority != AgentTurnSettlementLive || persisted.result.AgentTurnID != lease.ID || persisted.executionEpoch != lease.ExecutionEpoch ||
		persisted.controlRevision != lease.ControlRevision || persisted.workflowAttemptID != lease.WorkflowAttemptID ||
		persisted.assignmentID != lease.AgentAssignmentID || persisted.sessionID != lease.AgentSessionID ||
		persisted.executionJobID != lease.JobLease.ID || persisted.jobAttempt != lease.JobLease.Attempt ||
		persisted.jobLeaseOwner != lease.JobLease.LeaseOwner || persisted.jobLeaseToken != lease.JobLease.LeaseToken ||
		persisted.ownerID != lease.OwnerID || !bytes.Equal(persisted.ownerTokenSHA256, ownerHash[:]) {
		return ErrAgentTurnFenceLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, persisted.executionJobID))
	if err != nil || !sameAgentTurnExecutionIdentity(job, lease) {
		return ErrAgentTurnFenceLost
	}
	persistedObservation, persistedErr := canonicalJSON(persisted.observation)
	replayedObservation, replayedErr := canonicalJSON(observation)
	if persistedErr != nil || replayedErr != nil || !bytes.Equal(persisted.observationSHA256, observationHash[:]) || !bytes.Equal(persistedObservation, replayedObservation) {
		return ErrAgentTurnSettlementConflict
	}
	return nil
}
