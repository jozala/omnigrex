package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	// ErrAssignmentCollectionFenceLost means collection ownership or immutable generation identity is stale.
	ErrAssignmentCollectionFenceLost = errors.New("assignment collection fence lost")
	// ErrAssignmentCollectionNotDue means the authoritative retention deadline has not elapsed.
	ErrAssignmentCollectionNotDue = errors.New("assignment collection is not due")
	// ErrAssignmentCollectionIncomplete means the cleaner did not confirm every authorized target absent.
	ErrAssignmentCollectionIncomplete = errors.New("assignment collection confirmation is incomplete")
	// ErrAssignmentCollectionIrrevocable means physical deletion was already authorized for this generation.
	ErrAssignmentCollectionIrrevocable = errors.New("assignment collection is irrevocably authorized")
	// ErrInvalidAssignmentCleanupTargets means deletion cannot be safely authorized for the captured target set.
	ErrInvalidAssignmentCleanupTargets = errors.New("invalid Assignment collection cleanup targets")
)

const CollectAssignmentsJobKind = "COLLECT_ASSIGNMENTS"

type retentionGenerationStatus string

const (
	retentionScheduled  retentionGenerationStatus = "SCHEDULED"
	retentionCollecting retentionGenerationStatus = "COLLECTING"
	retentionCollected  retentionGenerationStatus = "COLLECTED"
	retentionCancelled  retentionGenerationStatus = "CANCELLED"
)

// AssignmentCleanupTarget is one immutable Runtime State deletion target.
// SessionID is empty for the Assignment-level target.
type AssignmentCleanupTarget struct {
	AssignmentID       string `json:"assignment_id"`
	SessionID          string `json:"session_id,omitempty"`
	RuntimeStatePath   string `json:"runtime_state_path"`
	RuntimeImageDigest string `json:"runtime_image_digest"`
}

// AssignmentCollectionAuthorization grants physical deletion authority for one immutable generation.
type AssignmentCollectionAuthorization struct {
	GenerationID      string
	WorkflowID        string
	RetentionToken    string
	RetentionDeadline time.Time
	WorkflowRevision  uint64
	Targets           []AssignmentCleanupTarget
}

// AssignmentCollection records a finalized or previously finalized generation.
type AssignmentCollection struct {
	GenerationID            string
	WorkflowID              string
	RetentionToken          string
	WorkflowRevision        uint64
	AppliedWorkflowRevision uint64
	CollectedAt             time.Time
}

// AcknowledgeAssignmentCollectionFailure terminally hands off unsafe targets or preserves retry authority after authorization.
func (store *Store) AcknowledgeAssignmentCollectionFailure(ctx context.Context, lease JobLease, cause error, retryable bool, retryDelay time.Duration) (WorkflowActionFailureAcknowledgement, error) {
	if err := validateWorkflowActionFailure(cause, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("begin Assignment collection failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRetentionWorkflow(ctx, tx, lease); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	job, err := lockFencedWorkflowJob(ctx, tx, lease, CollectAssignmentsJobKind, ErrAssignmentCollectionFenceLost)
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	payload, generation, err := lockRetentionGeneration(ctx, tx, job)
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	retryScheduled := retryable && job.AttemptCount < job.MaxAttempts
	acknowledgement := WorkflowActionFailureAcknowledgement{JobID: job.ID, WorkflowID: job.WorkflowID}
	invalidTargets := errors.Is(cause, ErrInvalidAssignmentCleanupTargets)
	if invalidTargets {
		if generation.Status != retentionScheduled {
			return WorkflowActionFailureAcknowledgement{}, ErrAssignmentCollectionFenceLost
		}
		targets, err := readRetentionTargets(ctx, tx, generation.ID)
		if err != nil {
			return WorkflowActionFailureAcknowledgement{}, err
		}
		validationErr := validateDurableAssignmentCleanupTargets(ctx, tx, payload, generation, targets)
		if validationErr == nil {
			return WorkflowActionFailureAcknowledgement{}, ErrAssignmentCollectionFenceLost
		}
		if !errors.Is(validationErr, ErrInvalidAssignmentCleanupTargets) {
			return WorkflowActionFailureAcknowledgement{}, validationErr
		}
		retryable, retryScheduled = false, false
		scheduled, err := enqueueSafetyHandoffTx(ctx, tx, job, retentionInvalidTargetsHandoffReason, retentionInvalidTargetsDiagnostic)
		if err != nil {
			return WorkflowActionFailureAcknowledgement{}, err
		}
		acknowledgement.EscalationScheduled = scheduled
	} else if generation.Status == retentionCollecting {
		retryScheduled = true
		if job.AttemptCount >= job.MaxAttempts {
			continued, scheduled, err := continueIrreversibleJobAfterExhaustionTx(ctx, tx, job)
			if err != nil {
				return WorkflowActionFailureAcknowledgement{}, err
			}
			if !continued {
				return WorkflowActionFailureAcknowledgement{}, ErrAssignmentCollectionFenceLost
			}
			acknowledgement.EscalationScheduled = scheduled
		}
	} else if generation.Status != retentionScheduled {
		return WorkflowActionFailureAcknowledgement{}, ErrAssignmentCollectionFenceLost
	}
	acknowledgement.RetryScheduled = retryScheduled
	if err := failWorkflowActionJobTx(ctx, tx, job, cause.Error(), generation.Status == retentionCollecting || retryable, retryScheduled, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, ErrAssignmentCollectionFenceLost
	}
	if err := tx.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, job.WorkflowID).Scan(&acknowledgement.WorkflowRevision); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("commit Assignment collection failure acknowledgement: %w", err)
	}
	return acknowledgement, nil
}

type collectAssignmentsPayload struct {
	GenerationID     string    `json:"generation_id"`
	WorkflowID       string    `json:"workflow_id"`
	RetentionToken   string    `json:"retention_token"`
	RetainUntil      time.Time `json:"retain_until"`
	WorkflowRevision uint64    `json:"revision"`
	AssignmentIDs    []string  `json:"assignment_ids"`
}

type lockedRetentionGeneration struct {
	ID, WorkflowID, RetentionToken, CollectionJobID string
	Deadline                                        time.Time
	WorkflowRevision                                uint64
	Status                                          retentionGenerationStatus
	AuthorizedAttempt                               int
	AuthorizedOwner, AuthorizedToken                string
	AuthorizedAt, CollectedAt                       *time.Time
}

func completeCurrentAssignmentsTx(ctx context.Context, tx pgx.Tx, workflowID string, snapshot workflow.Snapshot) error {
	var retentionUntil any
	if snapshot.State == workflow.StateClosed && !snapshot.Assignments.RetainedUntil.IsZero() {
		retentionUntil = snapshot.Assignments.RetainedUntil
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'COMPLETED', completed_at = COALESCE(completed_at, clock_timestamp()),
    retention_until = $2, updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status <> 'SUPERSEDED' AND state_deleted_at IS NULL`, workflowID, retentionUntil); err != nil {
		return fmt.Errorf("complete current Agent Assignments: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_sessions AS session
SET status = 'RETAINED', retained_at = COALESCE(retained_at, clock_timestamp()),
    updated_at = clock_timestamp()
FROM agent_assignments AS assignment
WHERE assignment.workflow_id = $1 AND assignment.id = session.agent_assignment_id
  AND assignment.status = 'COMPLETED' AND assignment.state_deleted_at IS NULL
  AND session.state_deleted_at IS NULL
  AND session.status IN ('CREATING', 'ACTIVE', 'RETAINED')`, workflowID); err != nil {
		return fmt.Errorf("retain established Agent Sessions: %w", err)
	}
	return nil
}

func scheduleAssignmentRetentionTx(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, internalEventID, workflowID string, revision uint64, action workflow.ScheduleRetentionAction) error {
	generationID, err := randomUUID()
	if err != nil {
		return err
	}
	jobID, err := randomUUID()
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
SELECT id::text
FROM agent_assignments
WHERE workflow_id = $1 AND status = 'COMPLETED' AND state_deleted_at IS NULL
ORDER BY id FOR UPDATE`, workflowID)
	if err != nil {
		return fmt.Errorf("lock retained Agent Assignments: %w", err)
	}
	var assignmentIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		assignmentIDs = append(assignmentIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(assignmentIDs) == 0 {
		return errors.New("schedule assignment retention: no retained Agent Assignments")
	}
	payload, err := json.Marshal(collectAssignmentsPayload{
		GenerationID: generationID, WorkflowID: workflowID, RetentionToken: action.RetentionToken,
		RetainUntil: action.RetainUntil, WorkflowRevision: revision, AssignmentIDs: assignmentIDs,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO assignment_retention_generations (
    id, workflow_id, retention_token, retention_deadline, workflow_revision,
    status, collection_job_id
)
VALUES ($1, $2, $3, $4, $5, 'SCHEDULED', $6)`, generationID, workflowID,
		action.RetentionToken, action.RetainUntil, int64(revision), jobID); err != nil {
		return fmt.Errorf("create Assignment retention generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO assignment_retention_targets (
    retention_generation_id, assignment_id, session_id,
    runtime_state_path, runtime_image_digest
)
SELECT $1::uuid, assignment.id, NULL::uuid, assignment.runtime_state_path, assignment.runtime_image_digest
FROM agent_assignments AS assignment
WHERE assignment.id = ANY($2::uuid[])
UNION ALL
SELECT $1::uuid, assignment.id, session.id, session.runtime_state_path, session.runtime_image_digest
FROM agent_assignments AS assignment
JOIN agent_sessions AS session ON session.agent_assignment_id = assignment.id
WHERE assignment.id = ANY($2::uuid[]) AND session.status = 'RETAINED'
  AND session.state_deleted_at IS NULL`, generationID, assignmentIDs); err != nil {
		return fmt.Errorf("capture Assignment retention targets: %w", err)
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at,
    max_attempts, idempotency_key, workflow_id, normalized_event_id,
    agent_turn_settlement_id, workflow_internal_event_id, action_key,
    retention_generation_id
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, $5, 3, $6, $7, $8, $9, $10,
        'collect-retention', $11)`, jobID, WorkflowActionQueue, CollectAssignmentsJobKind,
		canonical, action.RetainUntil,
		workflowActionIdempotencyKey(workflowID, deliveryID, settlementID, internalEventID, "collect-retention"),
		workflowID, nullableString(deliveryID), nullableString(settlementID), nullableString(internalEventID), generationID); err != nil {
		return fmt.Errorf("enqueue Assignment collection: %w", err)
	}
	return nil
}

func cancelAssignmentRetentionTx(ctx context.Context, tx pgx.Tx, workflowID, token string) error {
	var generation lockedRetentionGeneration
	err := tx.QueryRow(ctx, `
SELECT id::text, workflow_id::text, retention_token, retention_deadline,
       workflow_revision, status, collection_job_id::text,
       COALESCE(authorized_attempt_number, 0), COALESCE(authorized_lease_owner, ''),
       COALESCE(authorized_lease_token::text, ''), authorized_at, collected_at
FROM assignment_retention_generations
WHERE workflow_id = $1 AND retention_token = $2 FOR UPDATE`, workflowID, token).Scan(
		&generation.ID, &generation.WorkflowID, &generation.RetentionToken, &generation.Deadline,
		&generation.WorkflowRevision, &generation.Status, &generation.CollectionJobID,
		&generation.AuthorizedAttempt, &generation.AuthorizedOwner, &generation.AuthorizedToken,
		&generation.AuthorizedAt, &generation.CollectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock Assignment retention cancellation: %w", err)
	}
	switch generation.Status {
	case retentionCancelled:
		return nil
	case retentionCollecting:
		return ErrAssignmentCollectionIrrevocable
	case retentionCollected:
		return nil
	case retentionScheduled:
	default:
		return ErrAssignmentCollectionFenceLost
	}
	var jobStatus JobStatus
	var attempt int
	var leaseToken string
	if err := tx.QueryRow(ctx, `SELECT status, attempt_count, COALESCE(lease_token::text, '') FROM jobs WHERE id = $1 FOR UPDATE`, generation.CollectionJobID).Scan(&jobStatus, &attempt, &leaseToken); err != nil {
		return fmt.Errorf("lock cancelled collection Job: %w", err)
	}
	if jobStatus == JobLeased {
		if _, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'FAILED', finished_at = clock_timestamp(),
    retryable = FALSE, last_error = 'Assignment retention cancelled before collection authorization'
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`,
			generation.CollectionJobID, attempt, leaseToken); err != nil {
			return err
		}
	}
	if jobStatus == JobAvailable || jobStatus == JobLeased {
		if _, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Assignment retention cancelled before collection authorization'
WHERE id = $1`, generation.CollectionJobID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET retention_until = NULL, updated_at = clock_timestamp()
WHERE id IN (
    SELECT assignment_id FROM assignment_retention_targets
    WHERE retention_generation_id = $1
) AND state_deleted_at IS NULL`, generation.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE assignment_retention_generations
SET status = 'CANCELLED', cancelled_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1 AND status = 'SCHEDULED'`, generation.ID); err != nil {
		return err
	}
	return nil
}

// AuthorizeAssignmentCollection irreversibly authorizes deletion under the Workflow and live Job lease fences.
func (store *Store) AuthorizeAssignmentCollection(ctx context.Context, lease JobLease) (AssignmentCollectionAuthorization, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AssignmentCollectionAuthorization{}, fmt.Errorf("begin Assignment collection authorization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRetentionWorkflow(ctx, tx, lease); err != nil {
		return AssignmentCollectionAuthorization{}, err
	}
	job, err := lockFencedWorkflowJob(ctx, tx, lease, CollectAssignmentsJobKind, ErrAssignmentCollectionFenceLost)
	if err != nil {
		return AssignmentCollectionAuthorization{}, err
	}
	payload, generation, err := lockRetentionGeneration(ctx, tx, job)
	if err != nil {
		return AssignmentCollectionAuthorization{}, err
	}
	targets, err := readRetentionTargets(ctx, tx, generation.ID)
	if err != nil {
		return AssignmentCollectionAuthorization{}, err
	}
	if generation.Status == retentionScheduled {
		if err := validateDurableAssignmentCleanupTargets(ctx, tx, payload, generation, targets); err != nil {
			return AssignmentCollectionAuthorization{}, err
		}
		var dueAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dueAt); err != nil {
			return AssignmentCollectionAuthorization{}, err
		}
		if dueAt.Before(generation.Deadline) {
			return AssignmentCollectionAuthorization{}, ErrAssignmentCollectionNotDue
		}
		var state workflow.State
		var revision uint64
		var token string
		var deadline *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, state_revision, COALESCE(retention_token, ''), retention_deadline FROM workflows WHERE id = $1`, job.WorkflowID).Scan(&state, &revision, &token, &deadline); err != nil || state != workflow.StateClosed || revision != payload.WorkflowRevision || token != payload.RetentionToken || deadline == nil || !deadline.Equal(payload.RetainUntil) {
			return AssignmentCollectionAuthorization{}, ErrAssignmentCollectionFenceLost
		}
		if _, err := tx.Exec(ctx, `
UPDATE assignment_retention_generations
SET status = 'COLLECTING', authorized_attempt_number = $2,
    authorized_lease_owner = $3, authorized_lease_token = $4,
    authorized_at = $5, updated_at = clock_timestamp()
WHERE id = $1 AND status = 'SCHEDULED'`, generation.ID, job.AttemptCount,
			job.LeaseOwner, job.LeaseToken, dueAt); err != nil {
			return AssignmentCollectionAuthorization{}, err
		}
		generation.Status = retentionCollecting
		generation.AuthorizedAttempt = job.AttemptCount
		generation.AuthorizedOwner = job.LeaseOwner
		generation.AuthorizedToken = job.LeaseToken
		generation.AuthorizedAt = &dueAt
	} else if generation.Status == retentionCollecting {
		if generation.AuthorizedAttempt != job.AttemptCount || generation.AuthorizedOwner != job.LeaseOwner || generation.AuthorizedToken != job.LeaseToken {
			if _, err := tx.Exec(ctx, `
UPDATE assignment_retention_generations
SET authorized_attempt_number = $2, authorized_lease_owner = $3,
    authorized_lease_token = $4, updated_at = clock_timestamp()
WHERE id = $1 AND status = 'COLLECTING'`, generation.ID, job.AttemptCount,
				job.LeaseOwner, job.LeaseToken); err != nil {
				return AssignmentCollectionAuthorization{}, err
			}
			generation.AuthorizedAttempt = job.AttemptCount
			generation.AuthorizedOwner = job.LeaseOwner
			generation.AuthorizedToken = job.LeaseToken
		}
	} else {
		return AssignmentCollectionAuthorization{}, ErrAssignmentCollectionFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignmentCollectionAuthorization{}, fmt.Errorf("commit Assignment collection authorization: %w", err)
	}
	return AssignmentCollectionAuthorization{
		GenerationID: generation.ID, WorkflowID: generation.WorkflowID,
		RetentionToken: generation.RetentionToken, RetentionDeadline: generation.Deadline,
		WorkflowRevision: generation.WorkflowRevision, Targets: targets,
	}, nil
}

// FinalizeAssignmentCollection records deletion only after every authorized target is confirmed absent.
func (store *Store) FinalizeAssignmentCollection(ctx context.Context, lease JobLease, confirmedAbsent []AssignmentCleanupTarget) (AssignmentCollection, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AssignmentCollection{}, fmt.Errorf("begin Assignment collection finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRetentionWorkflow(ctx, tx, lease); err != nil {
		return AssignmentCollection{}, err
	}
	if completed, ok, err := resolveFinalizedCollection(ctx, tx, lease); err != nil || ok {
		if err != nil {
			return AssignmentCollection{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AssignmentCollection{}, fmt.Errorf("commit resolved Assignment collection: %w", err)
		}
		return completed, nil
	}
	job, err := lockFencedWorkflowJob(ctx, tx, lease, CollectAssignmentsJobKind, ErrAssignmentCollectionFenceLost)
	if err != nil {
		return AssignmentCollection{}, err
	}
	payload, generation, err := lockRetentionGeneration(ctx, tx, job)
	if err != nil || generation.Status != retentionCollecting || generation.AuthorizedAttempt != job.AttemptCount || generation.AuthorizedOwner != job.LeaseOwner || generation.AuthorizedToken != job.LeaseToken {
		return AssignmentCollection{}, ErrAssignmentCollectionFenceLost
	}
	targets, err := readRetentionTargets(ctx, tx, generation.ID)
	if err != nil {
		return AssignmentCollection{}, err
	}
	if !equalCleanupTargets(targets, confirmedAbsent) {
		return AssignmentCollection{}, ErrAssignmentCollectionIncomplete
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return AssignmentCollection{}, err
	}
	var collectedAt time.Time
	if generation.AuthorizedAt == nil {
		return AssignmentCollection{}, ErrAssignmentCollectionFenceLost
	}
	if err := tx.QueryRow(ctx, `SELECT GREATEST(clock_timestamp(), $1::timestamptz, $2::timestamptz)`,
		*generation.AuthorizedAt, generation.Deadline).Scan(&collectedAt); err != nil {
		return AssignmentCollection{}, err
	}
	internalEventID, err := randomUUID()
	if err != nil {
		return AssignmentCollection{}, err
	}
	event := workflow.AssignmentsCollectedEvent{EventMetadata: workflow.EventMetadata{
		ID: internalEventID, ObservedAt: collectedAt, WorkItem: snapshot.WorkItem,
		ExpectedRevision: snapshot.Revision,
	}, RetentionToken: payload.RetentionToken, RetainUntil: payload.RetainUntil, CollectedAt: collectedAt}
	decision := workflow.Reduce(snapshot, event)
	if err := validateWorkflowDecision(snapshot, decision); err != nil || decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonAssignmentsCollected {
		return AssignmentCollection{}, ErrAssignmentCollectionFenceLost
	}
	eventPayload, err := json.Marshal(map[string]any{
		"generation_id": generation.ID, "retention_token": payload.RetentionToken,
		"retain_until": payload.RetainUntil, "collected_at": collectedAt,
	})
	if err != nil {
		return AssignmentCollection{}, err
	}
	if err := insertInternalEventTx(ctx, tx, internalEventID, job, workflow.EventKindAssignmentsCollected, eventPayload, collectedAt); err != nil {
		return AssignmentCollection{}, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, json.RawMessage(`{"assignments_collected":true}`), ErrAssignmentCollectionFenceLost); err != nil {
		return AssignmentCollection{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET status = 'DELETED', state_deleted_at = COALESCE(state_deleted_at, $2),
    updated_at = clock_timestamp()
WHERE id IN (
    SELECT session_id FROM assignment_retention_targets
    WHERE retention_generation_id = $1 AND session_id IS NOT NULL
) AND state_deleted_at IS NULL`, generation.ID, collectedAt); err != nil {
		return AssignmentCollection{}, fmt.Errorf("mark Agent Sessions deleted: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET state_deleted_at = COALESCE(state_deleted_at, $2), retention_until = NULL,
    updated_at = clock_timestamp()
WHERE id IN (
    SELECT assignment_id FROM assignment_retention_targets
    WHERE retention_generation_id = $1
) AND state_deleted_at IS NULL`, generation.ID, collectedAt); err != nil {
		return AssignmentCollection{}, fmt.Errorf("mark Agent Assignments deleted: %w", err)
	}
	if err := persistAppliedDecisionWithInternalProvenance(ctx, tx, "", "", internalEventID, job.WorkflowID, snapshot, decision, ""); err != nil {
		return AssignmentCollection{}, err
	}
	if err := completeInternalEventTx(ctx, tx, internalEventID, decision); err != nil {
		return AssignmentCollection{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE assignment_retention_generations
SET status = 'COLLECTED', collected_event_id = $2, collected_at = $3,
    updated_at = clock_timestamp()
WHERE id = $1 AND status = 'COLLECTING'`, generation.ID, internalEventID, collectedAt); err != nil {
		return AssignmentCollection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignmentCollection{}, fmt.Errorf("commit Assignment collection finalization: %w", err)
	}
	return AssignmentCollection{GenerationID: generation.ID, WorkflowID: generation.WorkflowID,
		RetentionToken: generation.RetentionToken, WorkflowRevision: generation.WorkflowRevision,
		AppliedWorkflowRevision: decision.Snapshot.Revision,
		CollectedAt:             collectedAt}, nil
}

func lockRetentionWorkflow(ctx context.Context, tx pgx.Tx, lease JobLease) error {
	if !validUUID(lease.WorkflowID) {
		return ErrAssignmentCollectionFenceLost
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 FOR UPDATE`, lease.WorkflowID).Scan(&id); err != nil || id != lease.WorkflowID {
		return ErrAssignmentCollectionFenceLost
	}
	return nil
}

func lockRetentionGeneration(ctx context.Context, tx pgx.Tx, job Job) (collectAssignmentsPayload, lockedRetentionGeneration, error) {
	var payload collectAssignmentsPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.GenerationID == "" || payload.WorkflowID != job.WorkflowID || payload.RetentionToken == "" || payload.RetainUntil.IsZero() || payload.WorkflowRevision == 0 || len(payload.AssignmentIDs) == 0 {
		return collectAssignmentsPayload{}, lockedRetentionGeneration{}, ErrAssignmentCollectionFenceLost
	}
	var generation lockedRetentionGeneration
	err := tx.QueryRow(ctx, `
SELECT id::text, workflow_id::text, retention_token, retention_deadline,
       workflow_revision, status, collection_job_id::text,
       COALESCE(authorized_attempt_number, 0), COALESCE(authorized_lease_owner, ''),
       COALESCE(authorized_lease_token::text, ''), authorized_at, collected_at
FROM assignment_retention_generations WHERE id = $1 FOR UPDATE`, payload.GenerationID).Scan(
		&generation.ID, &generation.WorkflowID, &generation.RetentionToken, &generation.Deadline,
		&generation.WorkflowRevision, &generation.Status, &generation.CollectionJobID,
		&generation.AuthorizedAttempt, &generation.AuthorizedOwner, &generation.AuthorizedToken,
		&generation.AuthorizedAt, &generation.CollectedAt)
	if err != nil || generation.WorkflowID != job.WorkflowID || generation.CollectionJobID != job.ID ||
		generation.RetentionToken != payload.RetentionToken || !generation.Deadline.Equal(payload.RetainUntil) ||
		generation.WorkflowRevision != payload.WorkflowRevision {
		return collectAssignmentsPayload{}, lockedRetentionGeneration{}, ErrAssignmentCollectionFenceLost
	}
	if generation.Status != retentionScheduled {
		targets, err := readRetentionTargets(ctx, tx, generation.ID)
		if err != nil {
			return collectAssignmentsPayload{}, lockedRetentionGeneration{}, err
		}
		assignmentSet := make(map[string]struct{})
		for _, target := range targets {
			assignmentSet[target.AssignmentID] = struct{}{}
		}
		actualIDs := make([]string, 0, len(assignmentSet))
		for id := range assignmentSet {
			actualIDs = append(actualIDs, id)
		}
		sort.Strings(actualIDs)
		wantIDs := append([]string(nil), payload.AssignmentIDs...)
		sort.Strings(wantIDs)
		if !reflect.DeepEqual(actualIDs, wantIDs) {
			return collectAssignmentsPayload{}, lockedRetentionGeneration{}, ErrAssignmentCollectionFenceLost
		}
	}
	return payload, generation, nil
}

func readRetentionTargets(ctx context.Context, tx pgx.Tx, generationID string) ([]AssignmentCleanupTarget, error) {
	rows, err := tx.Query(ctx, `
SELECT assignment_id::text, COALESCE(session_id::text, ''), runtime_state_path,
       runtime_image_digest
FROM assignment_retention_targets
WHERE retention_generation_id = $1
ORDER BY assignment_id, session_id NULLS FIRST
FOR SHARE`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []AssignmentCleanupTarget
	for rows.Next() {
		var target AssignmentCleanupTarget
		if err := rows.Scan(&target.AssignmentID, &target.SessionID, &target.RuntimeStatePath, &target.RuntimeImageDigest); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ValidateAssignmentCleanupTargets verifies that a complete immutable target set maps to canonical Assignment paths.
func ValidateAssignmentCleanupTargets(targets []AssignmentCleanupTarget) error {
	if len(targets) == 0 {
		return fmt.Errorf("%w: target set is empty", ErrInvalidAssignmentCleanupTargets)
	}
	type assignmentIdentity struct {
		path, imageDigest string
		hasRoot           bool
	}
	assignments := make(map[string]assignmentIdentity)
	for _, target := range targets {
		if !validCanonicalUUID(target.AssignmentID) || target.SessionID != "" && !validCanonicalUUID(target.SessionID) ||
			strings.TrimSpace(target.RuntimeImageDigest) == "" {
			return fmt.Errorf("%w: target identity is malformed", ErrInvalidAssignmentCleanupTargets)
		}
		canonicalPath := "assignment-" + target.AssignmentID + "/runtime-state"
		if target.RuntimeStatePath != canonicalPath {
			return fmt.Errorf("%w: runtime-state path is not canonical for its Assignment", ErrInvalidAssignmentCleanupTargets)
		}
		identity, exists := assignments[target.AssignmentID]
		if exists && (identity.path != target.RuntimeStatePath || identity.imageDigest != target.RuntimeImageDigest) {
			return fmt.Errorf("%w: Session target does not match its immutable Assignment", ErrInvalidAssignmentCleanupTargets)
		}
		if !exists {
			identity = assignmentIdentity{path: target.RuntimeStatePath, imageDigest: target.RuntimeImageDigest}
		}
		if target.SessionID == "" {
			if identity.hasRoot {
				return fmt.Errorf("%w: Assignment target is duplicated", ErrInvalidAssignmentCleanupTargets)
			}
			identity.hasRoot = true
		}
		assignments[target.AssignmentID] = identity
	}
	for _, identity := range assignments {
		if !identity.hasRoot {
			return fmt.Errorf("%w: Assignment-level target is missing", ErrInvalidAssignmentCleanupTargets)
		}
	}
	return nil
}

func validateDurableAssignmentCleanupTargets(ctx context.Context, tx pgx.Tx, payload collectAssignmentsPayload, generation lockedRetentionGeneration, targets []AssignmentCleanupTarget) error {
	if err := ValidateAssignmentCleanupTargets(targets); err != nil {
		return err
	}
	var valid bool
	err := tx.QueryRow(ctx, `
WITH payload_assignments AS MATERIALIZED (
    SELECT DISTINCT assignment_id
    FROM unnest($3::uuid[]) AS payload(assignment_id)
), required_assignments AS MATERIALIZED (
    SELECT assignment.id AS assignment_id, assignment.runtime_state_path,
           assignment.runtime_image_digest
    FROM agent_assignments AS assignment
    WHERE assignment.workflow_id = $2
      AND assignment.status = 'COMPLETED'
      AND assignment.state_deleted_at IS NULL
    FOR UPDATE OF assignment
), required_sessions AS MATERIALIZED (
    SELECT session.id AS session_id,
           session.agent_assignment_id AS assignment_id,
           session.runtime_state_path, session.runtime_image_digest
    FROM agent_sessions AS session
    JOIN required_assignments AS assignment
      ON assignment.assignment_id = session.agent_assignment_id
    WHERE session.state_deleted_at IS NULL
    FOR SHARE OF session
), targets AS MATERIALIZED (
    SELECT target.assignment_id, target.session_id,
           target.runtime_state_path, target.runtime_image_digest
    FROM assignment_retention_targets AS target
    WHERE target.retention_generation_id = $1
    FOR SHARE OF target
)
SELECT
    (SELECT count(*) FROM payload_assignments) = cardinality($3::uuid[])
    AND NOT EXISTS (
        SELECT assignment_id FROM payload_assignments
        EXCEPT
        SELECT assignment_id FROM required_assignments
    )
    AND NOT EXISTS (
        SELECT assignment_id FROM required_assignments
        EXCEPT
        SELECT assignment_id FROM payload_assignments
    )
    AND NOT EXISTS (
        SELECT 1
        FROM required_assignments AS assignment
        WHERE NOT EXISTS (
            SELECT 1 FROM targets AS target
            WHERE target.assignment_id = assignment.assignment_id
              AND target.session_id IS NULL
              AND target.runtime_state_path = assignment.runtime_state_path
              AND target.runtime_image_digest = assignment.runtime_image_digest
        )
    )
    AND NOT EXISTS (
        SELECT 1
        FROM required_sessions AS session
        WHERE NOT EXISTS (
            SELECT 1 FROM targets AS target
            WHERE target.assignment_id = session.assignment_id
              AND target.session_id = session.session_id
              AND target.runtime_state_path = session.runtime_state_path
              AND target.runtime_image_digest = session.runtime_image_digest
        )
    )
    AND NOT EXISTS (
        SELECT 1
        FROM targets AS target
        LEFT JOIN required_assignments AS assignment
          ON assignment.assignment_id = target.assignment_id
        LEFT JOIN required_sessions AS session
          ON session.session_id = target.session_id
         AND session.assignment_id = target.assignment_id
        WHERE assignment.assignment_id IS NULL
           OR (target.session_id IS NULL AND (
               target.runtime_state_path <> assignment.runtime_state_path
               OR target.runtime_image_digest <> assignment.runtime_image_digest
           ))
           OR (target.session_id IS NOT NULL AND (
               session.session_id IS NULL
               OR target.runtime_state_path <> session.runtime_state_path
               OR target.runtime_image_digest <> session.runtime_image_digest
           ))
    )`, generation.ID, generation.WorkflowID, payload.AssignmentIDs).Scan(&valid)
	if err != nil {
		return fmt.Errorf("validate durable Assignment cleanup targets: %w", err)
	}
	if !valid {
		return fmt.Errorf("%w: target set does not match its durable Workflow hierarchy", ErrInvalidAssignmentCleanupTargets)
	}
	return nil
}

func validCanonicalUUID(value string) bool {
	if value == "00000000-0000-0000-0000-000000000000" || len(value) != 36 ||
		value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func equalCleanupTargets(left, right []AssignmentCleanupTarget) bool {
	if len(left) != len(right) {
		return false
	}
	key := func(target AssignmentCleanupTarget) string {
		return strings.Join([]string{target.AssignmentID, target.SessionID, target.RuntimeStatePath, target.RuntimeImageDigest}, "\x00")
	}
	leftKeys := make([]string, len(left))
	rightKeys := make([]string, len(right))
	for index := range left {
		leftKeys[index] = key(left[index])
	}
	for index := range right {
		rightKeys[index] = key(right[index])
	}
	sort.Strings(leftKeys)
	sort.Strings(rightKeys)
	return reflect.DeepEqual(leftKeys, rightKeys)
}

func insertInternalEventTx(ctx context.Context, tx pgx.Tx, id string, job Job, kind workflow.EventKind, payload json.RawMessage, observedAt time.Time) error {
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workflow_internal_events (
    id, workflow_id, kind, source_job_id, source_attempt_number,
    source_lease_token, payload, observed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, job.WorkflowID, kind, job.ID,
		job.AttemptCount, job.LeaseToken, canonical, observedAt); err != nil {
		return fmt.Errorf("record internal Workflow event: %w", err)
	}
	return nil
}

func completeInternalEventTx(ctx context.Context, tx pgx.Tx, id string, decision workflow.Decision) error {
	result, err := tx.Exec(ctx, `
UPDATE workflow_internal_events
SET disposition = $2, reason = $3, workflow_revision = $4, applied_at = clock_timestamp()
WHERE id = $1 AND applied_at IS NULL`, id, decision.Disposition, decision.Reason,
		int64(decision.Snapshot.Revision))
	if err != nil || result.RowsAffected() != 1 {
		return ErrWorkflowDecisionInvalid
	}
	return nil
}

func resolveFinalizedCollection(ctx context.Context, tx pgx.Tx, lease JobLease) (AssignmentCollection, bool, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 || strings.TrimSpace(lease.LeaseOwner) == "" {
		return AssignmentCollection{}, false, nil
	}
	var job Job
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil || job.Kind != CollectAssignmentsJobKind || job.Status != JobSucceeded ||
		job.WorkflowID != lease.WorkflowID || job.AttemptCount != lease.Attempt || !bytes.Equal(job.Payload, lease.Payload) {
		return AssignmentCollection{}, false, nil
	}
	var attemptStatus string
	if err := tx.QueryRow(ctx, `
SELECT status FROM job_attempts
WHERE job_id = $1 AND attempt_number = $2 AND lease_owner = $3 AND lease_token = $4`,
		lease.ID, lease.Attempt, lease.LeaseOwner, lease.LeaseToken).Scan(&attemptStatus); err != nil || attemptStatus != "SUCCEEDED" {
		return AssignmentCollection{}, false, nil
	}
	var result AssignmentCollection
	if err := tx.QueryRow(ctx, `
SELECT generation.id::text, generation.workflow_id::text, generation.retention_token,
	       generation.workflow_revision, event.workflow_revision, generation.collected_at
FROM assignment_retention_generations AS generation
JOIN workflow_internal_events AS event ON event.id = generation.collected_event_id
WHERE generation.collection_job_id = $1 AND generation.status = 'COLLECTED'
  AND event.source_job_id = $1`, lease.ID).Scan(&result.GenerationID, &result.WorkflowID,
		&result.RetentionToken, &result.WorkflowRevision, &result.AppliedWorkflowRevision,
		&result.CollectedAt); err != nil {
		return AssignmentCollection{}, false, nil
	}
	return result, true, nil
}
