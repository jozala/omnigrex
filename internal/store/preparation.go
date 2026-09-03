package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	// ErrAgentTurnPreparationFenceLost means preparation ownership or immutable intent is stale.
	ErrAgentTurnPreparationFenceLost = errors.New("agent turn preparation fence lost")
	// ErrAssignmentConfigurationConflict means an existing Assignment has a different immutable binding.
	ErrAssignmentConfigurationConflict = errors.New("agent assignment configuration conflict")
	// ErrAgentAssignmentNotFound means the requested Assignment does not exist.
	ErrAgentAssignmentNotFound = errors.New("agent assignment not found")
	// ErrAgentSessionACPConflict means an ACP identifier or capability binding disagrees with durable state.
	ErrAgentSessionACPConflict = errors.New("agent session ACP binding conflict")
	// ErrAgentSessionControlFenceLost means a control transfer used a stale revision or identity.
	ErrAgentSessionControlFenceLost = errors.New("agent session control fence lost")
	// ErrHumanPromptLeaseActive means a durable human prompt token already owns Session admission.
	ErrHumanPromptLeaseActive = errors.New("human prompt lease already active")
	// ErrHumanPromptAdmissionUncertain means an expired durable token may still represent running ACP work.
	ErrHumanPromptAdmissionUncertain = errors.New("human prompt admission outcome is uncertain")
	// ErrHumanPromptLeaseLost means a human prompt operation no longer owns its durable fence.
	ErrHumanPromptLeaseLost = errors.New("human prompt lease lost")
)

// AgentAssignmentStatus is the durable lifecycle state of an Assignment.
type AgentAssignmentStatus string

const (
	AgentAssignmentActive          AgentAssignmentStatus = "ACTIVE"
	AgentAssignmentWaitingForHuman AgentAssignmentStatus = "WAITING_FOR_HUMAN"
	AgentAssignmentCompleted       AgentAssignmentStatus = "COMPLETED"
	AgentAssignmentSuperseded      AgentAssignmentStatus = "SUPERSEDED"
)

// AgentSessionStatus is the durable lifecycle state of an Agent Session.
type AgentSessionStatus string

const (
	AgentSessionCreating AgentSessionStatus = "CREATING"
	AgentSessionActive   AgentSessionStatus = "ACTIVE"
	AgentSessionRetained AgentSessionStatus = "RETAINED"
	AgentSessionDeleted  AgentSessionStatus = "DELETED"
	AgentSessionFailed   AgentSessionStatus = "FAILED"
)

// SessionControlOwner identifies who may submit the next prompt to an Agent Session.
type SessionControlOwner string

const (
	SessionControlAutomation SessionControlOwner = "AUTOMATION"
	SessionControlHuman      SessionControlOwner = "HUMAN"
)

// AssignmentRuntimeBinding is the immutable profile identity pinned to an Assignment.
type AssignmentRuntimeBinding struct {
	AgentProfileName            string
	RuntimeProfileName          string
	RuntimeProfileVersion       string
	RuntimeProfileContentSHA256 string
	RuntimeImageDigest          string
}

// AgentProfileSnapshot is the credential-free mutable Agent Profile content used for one turn.
type AgentProfileSnapshot struct {
	CommitSHA     string
	ContentSHA256 []byte
	Config        json.RawMessage
}

// RolePreparation supplies one Role's immutable binding and per-turn profile snapshot.
type RolePreparation struct {
	Binding AssignmentRuntimeBinding
	Profile AgentProfileSnapshot
}

// AgentTurnPreparationSpec supplies preparations for the complete two-Role Assignment set.
type AgentTurnPreparationSpec struct {
	Developer RolePreparation
	Reviewer  RolePreparation
}

// AgentAssignment is the coordinator-facing durable Assignment record.
type AgentAssignment struct {
	AssignmentRuntimeBinding
	ID                            string
	createdByPreparationJobID     string
	reactivatedByPreparationJobID string
	WorkflowID                    string
	Role                          workflow.Role
	Generation                    int
	Status                        AgentAssignmentStatus
	RuntimeStatePath              string
	CreatedAt                     time.Time
	UpdatedAt                     time.Time
	CompletedAt                   *time.Time
	RetentionUntil                *time.Time
	StateDeletedAt                *time.Time
}

// AgentSession is the coordinator-facing durable Agent Session record.
type AgentSession struct {
	ID                          string
	AgentAssignmentID           string
	SessionNumber               int
	ACPSessionID                string
	RuntimeProfileName          string
	RuntimeProfileVersion       string
	RuntimeProfileContentSHA256 string
	RuntimeImageDigest          string
	RuntimeStatePath            string
	Capabilities                json.RawMessage
	Status                      AgentSessionStatus
	ControlOwner                SessionControlOwner
	ControlRevision             int64
	ControllerID                string
	ControlAcquiredAt           *time.Time
	HumanPromptToken            string
	HumanPromptLeasedAt         *time.Time
	HumanPromptLeaseExpiresAt   *time.Time
	HumanPromptHeartbeatAt      *time.Time
	NextTurnNumber              int64
	NextExecutionEpoch          int64
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
	ActivatedAt                 *time.Time
	RetainedAt                  *time.Time
	StateDeletedAt              *time.Time
}

// HumanPromptLease is the durable fence authorizing one human ACP prompt lifecycle.
type HumanPromptLease struct {
	AgentSessionID  string
	ControlRevision int64
	ACPSessionID    string
	Token           string
	LeasedAt        time.Time
	LeaseExpiresAt  time.Time
}

// AgentTurnPreparation is the fenced identity returned while a preparation job is live.
type AgentTurnPreparation struct {
	JobID             string
	WorkflowID        string
	WorkflowAttemptID string
	WorkflowRevision  int64
	Mode              workflow.AssignmentGeneration
	Role              workflow.Role
	Purpose           workflow.TurnPurpose
	ExpectedHeadSHA   string
	RetryOfTurnID     string
	ChangeProposalID  string
	Assignment        AgentAssignment
	Session           AgentSession
}

// AgentTurnPreparationCommit is the atomic turn allocation and execution-job result.
type AgentTurnPreparationCommit struct {
	Assignment AgentAssignment
	Session    AgentSession
	Turn       AgentTurn
	Job        Job
}

// AssignmentConfigurationHandoff is the durable outcome of acknowledging an immutable Assignment binding conflict.
type AssignmentConfigurationHandoff struct {
	JobID             string
	WorkflowID        string
	WorkflowAttemptID string
	Role              workflow.Role
	WorkflowRevision  uint64
}

// AgentTurnPreparationFailureAcknowledgement is the durable retry or terminal outcome of one failed preparation attempt.
type AgentTurnPreparationFailureAcknowledgement struct {
	JobID             string
	WorkflowID        string
	WorkflowAttemptID string
	Role              workflow.Role
	RetryScheduled    bool
	WorkflowRevision  uint64
}

type agentTurnPreparationPayload struct {
	Mode            workflow.AssignmentGeneration `json:"mode"`
	Role            workflow.Role                 `json:"role"`
	Purpose         workflow.TurnPurpose          `json:"purpose"`
	ExpectedHeadSHA string                        `json:"expected_head_sha"`
	RetryOfTurnID   string                        `json:"retry_of_turn_id"`
	Revision        int64                         `json:"revision"`
}

// PrepareAgentTurn atomically establishes the Assignment and Session, allocates the Turn,
// enqueues its execution Job, and acknowledges the preparation Job.
func (store *Store) PrepareAgentTurn(ctx context.Context, lease JobLease, spec AgentTurnPreparationSpec) (AgentTurnPreparationCommit, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
	}
	if err := validatePreparationSpec(spec); err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("begin agent turn preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockFencedWorkflowJob(ctx, tx, lease, PrepareAgentTurnJobKind, ErrAgentTurnPreparationFenceLost)
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	payload, err := decodeAgentTurnPreparationPayload(job.Payload)
	if err != nil || !validAgentTurnPreparationPayload(payload, job) {
		return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
	}
	rolePreparation := spec.Developer
	if payload.Role == workflow.RoleReviewer {
		rolePreparation = spec.Reviewer
	}
	profile := rolePreparation.Profile
	changeProposalID, err := lockPreparationWorkflow(ctx, tx, job, payload)
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	assignments, err := ensurePreparationAssignments(ctx, tx, job.ID, job.WorkflowID, payload.Mode, spec)
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	assignment := assignments[payload.Role]
	session, err := ensurePreparationSession(ctx, tx, assignment)
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if session.Status == AgentSessionRetained {
		if _, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET status = 'ACTIVE', retained_at = NULL, updated_at = clock_timestamp()
WHERE id = $1 AND status = 'RETAINED'`, session.ID); err != nil {
			return AgentTurnPreparationCommit{}, fmt.Errorf("reactivate retained agent session: %w", err)
		}
		session.Status = AgentSessionActive
		session.RetainedAt = nil
	}
	preparation := AgentTurnPreparation{
		JobID: job.ID, WorkflowID: job.WorkflowID, WorkflowAttemptID: job.WorkflowAttemptID,
		WorkflowRevision: payload.Revision, Mode: payload.Mode, Role: payload.Role,
		Purpose: payload.Purpose, ExpectedHeadSHA: payload.ExpectedHeadSHA, RetryOfTurnID: payload.RetryOfTurnID,
		ChangeProposalID: changeProposalID, Assignment: assignment, Session: session,
	}
	profileConfig, err := validateAgentProfileSnapshot(profile, preparation)
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if session.ControlOwner != SessionControlAutomation || session.ControlRevision <= 0 ||
		(session.Status != AgentSessionCreating && session.Status != AgentSessionActive) {
		return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)`, preparation.WorkflowID).Scan(&active); err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if active {
		return AgentTurnPreparationCommit{}, ErrAgentTurnActive
	}
	if preparation.RetryOfTurnID != "" {
		var retrySession, retryStatus string
		var retryActive, recoverySettled bool
		err := tx.QueryRow(ctx, `
SELECT agent_session_id::text, status, active, recovery_settled_at IS NOT NULL
FROM agent_turns WHERE id = $1 FOR UPDATE`, preparation.RetryOfTurnID).Scan(&retrySession, &retryStatus, &retryActive, &recoverySettled)
		if err != nil || retrySession != session.ID || retryActive || !retryableTurnStatus(AgentTurnStatus(retryStatus), recoverySettled) {
			return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
		}
	}
	var recoveryUnsettled, mutationsUnsettled bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_turns
    WHERE agent_session_id = $1 AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL
)`, session.ID).Scan(&recoveryUnsettled); err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("check Agent Turn preparation recovery barrier: %w", err)
	}
	if recoveryUnsettled {
		return AgentTurnPreparationCommit{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := tx.QueryRow(ctx, unsettledMutationsForSessionSQL, session.ID).Scan(&mutationsUnsettled); err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("check Agent Turn preparation mutations: %w", err)
	}
	if mutationsUnsettled {
		return AgentTurnPreparationCommit{}, ErrAgentTurnMutationsUnsettled
	}
	turnID, err := randomUUID()
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	executionJobID, err := randomUUID()
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	turnNumber := session.NextTurnNumber
	executionEpoch := session.NextExecutionEpoch
	if turnNumber <= 0 || executionEpoch <= 0 {
		return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
	}
	turn := AgentTurn{
		AgentTurnSpec: AgentTurnSpec{
			AgentSessionID: session.ID, WorkflowAttemptID: preparation.WorkflowAttemptID,
			RetryOfTurnID: preparation.RetryOfTurnID, Purpose: preparation.Purpose,
			ChangeProposalID: preparation.ChangeProposalID, ExpectedHeadSHA: preparation.ExpectedHeadSHA,
			ControlRevision: session.ControlRevision, AgentProfileCommitSHA: profile.CommitSHA,
			AgentProfileContentSHA256: append([]byte(nil), profile.ContentSHA256...), AgentProfileConfig: profileConfig,
		},
		ID: turnID, AgentAssignmentID: assignment.ID, TurnNumber: turnNumber,
		ExecutionEpoch: executionEpoch, Status: AgentTurnQueued,
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO agent_turns (
    id, workflow_id, preparation_job_id, agent_session_id, workflow_attempt_id,
    turn_number, execution_epoch, retry_of_turn_id, status, active, control_revision,
    agent_profile_commit_sha, agent_profile_content_sha256, agent_profile_config,
    purpose, change_proposal_id, expected_head_sha
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', TRUE, $9, $10, $11, $12, $13, $14, $15)
RETURNING created_at`, turn.ID, preparation.WorkflowID, job.ID, session.ID,
		preparation.WorkflowAttemptID, turn.TurnNumber, turn.ExecutionEpoch,
		nullableString(turn.RetryOfTurnID), turn.ControlRevision, turn.AgentProfileCommitSHA,
		turn.AgentProfileContentSHA256, turn.AgentProfileConfig, turn.Purpose,
		nullableString(turn.ChangeProposalID), nullableString(turn.ExpectedHeadSHA)).Scan(&turn.CreatedAt); err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("insert prepared Agent Turn: %w", err)
	}
	result, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET next_turn_number = next_turn_number + 1, next_execution_epoch = next_execution_epoch + 1,
    updated_at = clock_timestamp()
WHERE id = $1 AND control_owner = 'AUTOMATION' AND control_revision = $2
  AND status IN ('CREATING', 'ACTIVE')`, session.ID, session.ControlRevision)
	if err != nil || result.RowsAffected() != 1 {
		return AgentTurnPreparationCommit{}, ErrAgentTurnPreparationFenceLost
	}
	executionPayload, err := json.Marshal(map[string]any{
		"agent_turn_id": turn.ID, "agent_session_id": session.ID,
		"execution_epoch": turn.ExecutionEpoch, "control_revision": turn.ControlRevision,
	})
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, clock_timestamp(), 1,
        $5, $6, $7, $8, $9, $10, $11)`, executionJobID, AgentTurnQueue, RunAgentTurnJobKind,
		executionPayload, "run-agent-turn:"+turn.ID, preparation.WorkflowID, preparation.WorkflowAttemptID,
		assignment.ID, session.ID, turn.ID, turn.ExecutionEpoch); err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("enqueue prepared Agent Turn: %w", err)
	}
	jobResult, err := json.Marshal(map[string]any{
		"agent_assignment_id": assignment.ID, "agent_session_id": session.ID,
		"agent_turn_id": turnID, "execution_job_id": executionJobID, "execution_epoch": executionEpoch,
	})
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, jobResult, ErrAgentTurnPreparationFenceLost); err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	executionJob, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, executionJobID))
	if err != nil {
		return AgentTurnPreparationCommit{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnPreparationCommit{}, fmt.Errorf("commit agent turn preparation: %w", err)
	}
	session.NextTurnNumber++
	session.NextExecutionEpoch++
	return AgentTurnPreparationCommit{Assignment: assignment, Session: session, Turn: turn, Job: executionJob}, nil
}

// AcknowledgeAssignmentConfigurationConflict revalidates immutable binding drift and atomically creates a Human Handoff.
func (store *Store) AcknowledgeAssignmentConfigurationConflict(ctx context.Context, lease JobLease, spec AgentTurnPreparationSpec) (AssignmentConfigurationHandoff, error) {
	if err := validatePreparationSpec(spec); err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AssignmentConfigurationHandoff{}, fmt.Errorf("begin Assignment configuration conflict acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := lockFencedWorkflowJob(ctx, tx, lease, PrepareAgentTurnJobKind, ErrAgentTurnPreparationFenceLost)
	if err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	payload, err := decodeAgentTurnPreparationPayload(job.Payload)
	if err != nil || !validAgentTurnPreparationPayload(payload, job) {
		return AssignmentConfigurationHandoff{}, ErrAgentTurnPreparationFenceLost
	}
	if _, err := lockPreparationWorkflow(ctx, tx, job, payload); err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	conflict, err := revalidateAssignmentConfigurationConflict(ctx, tx, job, payload, spec)
	if err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	if !conflict {
		return AssignmentConfigurationHandoff{}, ErrAgentTurnPreparationFenceLost
	}

	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return AssignmentConfigurationHandoff{}, fmt.Errorf("rehydrate Assignment configuration conflict Workflow: %w", err)
	}
	decision := workflow.Reduce(snapshot, workflow.AssignmentConfigurationConflictEvent{
		EventMetadata: workflow.EventMetadata{
			ID: job.ID, ObservedAt: job.CreatedAt, WorkItem: snapshot.WorkItem,
			ExpectedRevision: uint64(payload.Revision),
		},
		Role: payload.Role,
	})
	if err := validateWorkflowDecision(snapshot, decision); err != nil || decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonAssignmentConfigurationConflict {
		return AssignmentConfigurationHandoff{}, ErrAgentTurnPreparationFenceLost
	}
	var normalizedEventID string
	if err := tx.QueryRow(ctx, `SELECT normalized_event_id::text FROM jobs WHERE id = $1`, job.ID).Scan(&normalizedEventID); err != nil {
		return AssignmentConfigurationHandoff{}, ErrAgentTurnPreparationFenceLost
	}
	actionNamespace := "prepare-agent-turn:" + job.ID
	if err := persistAppliedDecision(ctx, tx, normalizedEventID, job.WorkflowID, snapshot, decision, actionNamespace); err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'WAITING_FOR_HUMAN', completed_at = NULL, retention_until = NULL,
    updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status IN ('ACTIVE', 'COMPLETED', 'WAITING_FOR_HUMAN')
  AND state_deleted_at IS NULL`, job.WorkflowID)
	if err != nil {
		return AssignmentConfigurationHandoff{}, fmt.Errorf("mark Assignments waiting for Human Handoff: %w", err)
	}
	if updated.RowsAffected() != 2 {
		return AssignmentConfigurationHandoff{}, ErrAgentTurnPreparationFenceLost
	}
	jobResult, err := json.Marshal(map[string]any{
		"reason": decision.Reason, "revision": decision.Snapshot.Revision,
	})
	if err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, jobResult, ErrAgentTurnPreparationFenceLost); err != nil {
		return AssignmentConfigurationHandoff{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignmentConfigurationHandoff{}, fmt.Errorf("commit Assignment configuration conflict acknowledgement: %w", err)
	}
	return AssignmentConfigurationHandoff{
		JobID: job.ID, WorkflowID: job.WorkflowID, WorkflowAttemptID: job.WorkflowAttemptID,
		Role: payload.Role, WorkflowRevision: decision.Snapshot.Revision,
	}, nil
}

// AcknowledgeAgentTurnPreparationFailure records a failed preparation attempt and either retries it or creates a Human Handoff atomically.
func (store *Store) AcknowledgeAgentTurnPreparationFailure(ctx context.Context, lease JobLease, cause error, retryable bool, retryDelay time.Duration) (AgentTurnPreparationFailureAcknowledgement, error) {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return AgentTurnPreparationFailureAcknowledgement{}, errors.New("acknowledge Agent Turn preparation failure: cause is empty")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("acknowledge Agent Turn preparation failure: retry delay must be between zero and %s", maximumJobDelay)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("begin Agent Turn preparation failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := lockFencedWorkflowJob(ctx, tx, lease, PrepareAgentTurnJobKind, ErrAgentTurnPreparationFenceLost)
	if err != nil {
		return AgentTurnPreparationFailureAcknowledgement{}, err
	}
	payload, err := decodeAgentTurnPreparationPayload(job.Payload)
	if err != nil || !validAgentTurnPreparationPayload(payload, job) {
		return AgentTurnPreparationFailureAcknowledgement{}, ErrAgentTurnPreparationFenceLost
	}
	if _, err := lockPreparationWorkflow(ctx, tx, job, payload); err != nil {
		return AgentTurnPreparationFailureAcknowledgement{}, err
	}
	retryScheduled := retryable && job.AttemptCount < job.MaxAttempts
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = $4, last_error = $5
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, retryable, cause.Error())
	if err != nil || attemptResult.RowsAffected() != 1 {
		return AgentTurnPreparationFailureAcknowledgement{}, ErrAgentTurnPreparationFenceLost
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() + $5 * interval '1 microsecond' ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    last_error = $6
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, retryScheduled, retryDelay.Microseconds(), cause.Error())
	if err != nil || jobResult.RowsAffected() != 1 {
		return AgentTurnPreparationFailureAcknowledgement{}, ErrAgentTurnPreparationFenceLost
	}

	acknowledgement := AgentTurnPreparationFailureAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, WorkflowAttemptID: job.WorkflowAttemptID,
		Role: payload.Role, RetryScheduled: retryScheduled, WorkflowRevision: uint64(payload.Revision),
	}
	if !retryScheduled {
		snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
		if err != nil {
			return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("rehydrate Agent Turn preparation failure Workflow: %w", err)
		}
		var assignmentsExist bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_assignments
    WHERE workflow_id = $1 AND status <> 'SUPERSEDED' AND state_deleted_at IS NULL
)`, job.WorkflowID).Scan(&assignmentsExist); err != nil {
			return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("inspect preparation Assignments: %w", err)
		}
		decision := workflow.Reduce(snapshot, workflow.AgentTurnPreparationFailedEvent{
			EventMetadata: workflow.EventMetadata{
				ID: job.ID, ObservedAt: job.CreatedAt, WorkItem: snapshot.WorkItem,
				ExpectedRevision: uint64(payload.Revision),
			},
			Role: payload.Role, Diagnostic: cause.Error(), AssignmentsExist: assignmentsExist,
		})
		if err := validateWorkflowDecision(snapshot, decision); err != nil || decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonAgentTurnPreparationFailed {
			return AgentTurnPreparationFailureAcknowledgement{}, ErrAgentTurnPreparationFenceLost
		}
		var normalizedEventID string
		if err := tx.QueryRow(ctx, `SELECT normalized_event_id::text FROM jobs WHERE id = $1`, job.ID).Scan(&normalizedEventID); err != nil {
			return AgentTurnPreparationFailureAcknowledgement{}, ErrAgentTurnPreparationFenceLost
		}
		if err := persistAppliedDecision(ctx, tx, normalizedEventID, job.WorkflowID, snapshot, decision, "prepare-agent-turn:"+job.ID); err != nil {
			return AgentTurnPreparationFailureAcknowledgement{}, err
		}
		if _, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'WAITING_FOR_HUMAN', completed_at = NULL, retention_until = NULL,
    updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status IN ('ACTIVE', 'COMPLETED')
  AND state_deleted_at IS NULL`, job.WorkflowID); err != nil {
			return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("mark preparation Assignments waiting for Human Handoff: %w", err)
		}
		acknowledgement.WorkflowRevision = decision.Snapshot.Revision
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnPreparationFailureAcknowledgement{}, fmt.Errorf("commit Agent Turn preparation failure acknowledgement: %w", err)
	}
	return acknowledgement, nil
}

// BindAgentSessionACP persists the opaque ACP identity under a live Agent Turn lease.
func (store *Store) BindAgentSessionACP(ctx context.Context, lease AgentTurnLease, acpSessionID string, capabilities json.RawMessage) (AgentSession, error) {
	if strings.TrimSpace(acpSessionID) == "" {
		return AgentSession{}, errors.New("bind Agent Session ACP: session ID is empty")
	}
	canonicalCapabilities, err := canonicalJSON(capabilities)
	if err != nil {
		return AgentSession{}, fmt.Errorf("bind Agent Session ACP: capabilities: %w", err)
	}
	var bound AgentSession
	err = store.withLockedAgentTurnLeaseStatus(ctx, lease, "bind Agent Session ACP", true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1 FOR UPDATE`, lease.AgentSessionID))
		if err != nil {
			return err
		}
		if session.Status == AgentSessionCreating && session.ACPSessionID == "" {
			result, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET acp_session_id = $2, capabilities = $3, status = 'ACTIVE',
    activated_at = COALESCE(activated_at, clock_timestamp()), updated_at = clock_timestamp()
WHERE id = $1 AND status = 'CREATING' AND acp_session_id IS NULL`, session.ID, acpSessionID, canonicalCapabilities)
			if err != nil {
				return fmt.Errorf("bind Agent Session ACP: %w", err)
			}
			if result.RowsAffected() != 1 {
				return ErrAgentSessionACPConflict
			}
		} else if session.Status != AgentSessionActive || session.ACPSessionID != acpSessionID || !bytes.Equal(session.Capabilities, canonicalCapabilities) {
			return ErrAgentSessionACPConflict
		}
		bound, err = scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, session.ID))
		return err
	})
	return bound, err
}

// GetAgentAssignment returns one durable Assignment record.
func (store *Store) GetAgentAssignment(ctx context.Context, assignmentID string) (AgentAssignment, error) {
	if !validUUID(assignmentID) {
		return AgentAssignment{}, ErrAgentAssignmentNotFound
	}
	assignment, err := scanAgentAssignment(store.pool.QueryRow(ctx, agentAssignmentSelect+` WHERE id = $1`, assignmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentAssignment{}, ErrAgentAssignmentNotFound
	}
	if err != nil {
		return AgentAssignment{}, fmt.Errorf("get Agent Assignment: %w", err)
	}
	return assignment, nil
}

// ListAgentAssignments returns every Assignment generation for a Workflow.
func (store *Store) ListAgentAssignments(ctx context.Context, workflowID string) ([]AgentAssignment, error) {
	if !validUUID(workflowID) {
		return nil, ErrAgentAssignmentNotFound
	}
	rows, err := store.pool.Query(ctx, agentAssignmentSelect+` WHERE workflow_id = $1 ORDER BY generation, role`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("list Agent Assignments: %w", err)
	}
	defer rows.Close()
	var assignments []AgentAssignment
	for rows.Next() {
		assignment, err := scanAgentAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Agent Assignment: %w", err)
		}
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

// GetAgentSession returns one durable Agent Session record.
func (store *Store) GetAgentSession(ctx context.Context, sessionID string) (AgentSession, error) {
	if !validUUID(sessionID) {
		return AgentSession{}, ErrAgentSessionNotFound
	}
	session, err := scanAgentSession(store.pool.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentSession{}, ErrAgentSessionNotFound
	}
	if err != nil {
		return AgentSession{}, fmt.Errorf("get Agent Session: %w", err)
	}
	return session, nil
}

// GetAgentSessionForAssignment returns the current continuation-capable Session for an Assignment.
func (store *Store) GetAgentSessionForAssignment(ctx context.Context, assignmentID string) (AgentSession, error) {
	if !validUUID(assignmentID) {
		return AgentSession{}, ErrAgentSessionNotFound
	}
	session, err := scanAgentSession(store.pool.QueryRow(ctx, agentSessionSelect+`
 WHERE agent_assignment_id = $1 AND status IN ('CREATING', 'ACTIVE', 'RETAINED')`, assignmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentSession{}, ErrAgentSessionNotFound
	}
	if err != nil {
		return AgentSession{}, fmt.Errorf("get current Agent Session: %w", err)
	}
	return session, nil
}

// TransferAgentSessionControl compare-and-sets session authority and advances its revision once.
func (store *Store) TransferAgentSessionControl(ctx context.Context, sessionID string, expectedRevision int64, owner SessionControlOwner, controllerID string) (AgentSession, error) {
	if !validUUID(sessionID) || expectedRevision <= 0 ||
		(owner != SessionControlAutomation && owner != SessionControlHuman) || strings.TrimSpace(controllerID) == "" {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentSession{}, fmt.Errorf("begin Agent Session control transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workflowID, assignmentID string
	if err := tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, assignment.id::text
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = $1`, sessionID).Scan(&workflowID, &assignmentID); err != nil {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	var lockedWorkflowID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 FOR UPDATE`, workflowID).Scan(&lockedWorkflowID); err != nil {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	var assignmentStatus AgentAssignmentStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM agent_assignments WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, assignmentID, workflowID).Scan(&assignmentStatus); err != nil || assignmentStatus != AgentAssignmentActive {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1 AND agent_assignment_id = $2 FOR UPDATE`, sessionID, assignmentID))
	if err != nil || session.ControlRevision != expectedRevision ||
		session.Status != AgentSessionActive && session.Status != AgentSessionRetained {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	if session.HumanPromptToken != "" {
		return AgentSession{}, existingHumanPromptAdmissionError(ctx, tx, sessionID)
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)`, workflowID).Scan(&active); err != nil {
		return AgentSession{}, err
	}
	if active {
		return AgentSession{}, ErrAgentTurnActive
	}
	var recoveryUnsettled, mutationsUnsettled bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_turns
    WHERE agent_session_id = $1 AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL
)`, sessionID).Scan(&recoveryUnsettled); err != nil {
		return AgentSession{}, err
	}
	if recoveryUnsettled {
		return AgentSession{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := tx.QueryRow(ctx, unsettledMutationsForSessionSQL, sessionID).Scan(&mutationsUnsettled); err != nil {
		return AgentSession{}, err
	}
	if mutationsUnsettled {
		return AgentSession{}, ErrAgentTurnMutationsUnsettled
	}
	result, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET control_owner = $3, control_revision = control_revision + 1,
    controller_id = $4, control_acquired_at = clock_timestamp(), updated_at = clock_timestamp(),
    human_prompt_token = NULL, human_prompt_leased_at = NULL,
    human_prompt_lease_expires_at = NULL, human_prompt_heartbeat_at = NULL
WHERE id = $1 AND control_revision = $2`, sessionID, expectedRevision, owner, controllerID)
	if err != nil || result.RowsAffected() != 1 {
		return AgentSession{}, ErrAgentSessionControlFenceLost
	}
	session, err = scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, sessionID))
	if err != nil {
		return AgentSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentSession{}, fmt.Errorf("commit Agent Session control transfer: %w", err)
	}
	return session, nil
}

// AcquireHumanPromptLease atomically admits one human prompt under the exact durable Session fence.
func (store *Store) AcquireHumanPromptLease(ctx context.Context, sessionID string, expectedRevision int64, acpSessionID string, duration time.Duration) (HumanPromptLease, error) {
	if !validUUID(sessionID) || expectedRevision <= 0 || strings.TrimSpace(acpSessionID) == "" {
		return HumanPromptLease{}, ErrAgentSessionControlFenceLost
	}
	if err := validatePositiveDuration("human prompt lease", duration); err != nil {
		return HumanPromptLease{}, err
	}
	token, err := randomUUID()
	if err != nil {
		return HumanPromptLease{}, fmt.Errorf("create human prompt lease token: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return HumanPromptLease{}, fmt.Errorf("begin human prompt lease acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	workflowID, _, session, err := lockHumanPromptSession(ctx, tx, sessionID)
	if err != nil || session.ControlOwner != SessionControlHuman || session.ControlRevision != expectedRevision ||
		session.Status != AgentSessionActive && session.Status != AgentSessionRetained {
		return HumanPromptLease{}, ErrAgentSessionControlFenceLost
	}
	if session.ACPSessionID != acpSessionID {
		return HumanPromptLease{}, ErrAgentSessionACPConflict
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)`, workflowID).Scan(&active); err != nil {
		return HumanPromptLease{}, fmt.Errorf("check active Agent Turn before human prompt: %w", err)
	}
	if active {
		return HumanPromptLease{}, ErrAgentTurnActive
	}
	if session.HumanPromptToken != "" {
		return HumanPromptLease{}, existingHumanPromptAdmissionError(ctx, tx, sessionID)
	}
	lease := HumanPromptLease{AgentSessionID: sessionID, ControlRevision: expectedRevision, ACPSessionID: acpSessionID, Token: token}
	err = tx.QueryRow(ctx, `
WITH lease_time AS (SELECT clock_timestamp() AS now)
UPDATE agent_sessions AS session
SET human_prompt_token = $2, human_prompt_leased_at = lease_time.now,
    human_prompt_lease_expires_at = lease_time.now + $3 * interval '1 microsecond',
    human_prompt_heartbeat_at = lease_time.now, updated_at = lease_time.now
FROM lease_time
WHERE session.id = $1
RETURNING session.human_prompt_leased_at, session.human_prompt_lease_expires_at`,
		sessionID, token, duration.Microseconds()).Scan(&lease.LeasedAt, &lease.LeaseExpiresAt)
	if err != nil {
		return HumanPromptLease{}, fmt.Errorf("acquire human prompt lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return HumanPromptLease{}, fmt.Errorf("commit human prompt lease acquisition: %w", err)
	}
	return lease, nil
}

// HeartbeatHumanPromptLease extends a live lease while revalidating its complete durable fence.
func (store *Store) HeartbeatHumanPromptLease(ctx context.Context, lease HumanPromptLease, extension time.Duration) error {
	if !validUUID(lease.AgentSessionID) || !validUUID(lease.Token) || lease.ControlRevision <= 0 || strings.TrimSpace(lease.ACPSessionID) == "" {
		return ErrHumanPromptLeaseLost
	}
	if err := validatePositiveDuration("human prompt lease heartbeat", extension); err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin human prompt lease heartbeat: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	workflowID, _, session, err := lockHumanPromptSession(ctx, tx, lease.AgentSessionID)
	if err != nil || session.ControlOwner != SessionControlHuman || session.ControlRevision != lease.ControlRevision ||
		session.ACPSessionID != lease.ACPSessionID || session.HumanPromptToken != lease.Token ||
		session.Status != AgentSessionActive && session.Status != AgentSessionRetained {
		return ErrHumanPromptLeaseLost
	}
	var leaseLive, active bool
	if err := tx.QueryRow(ctx, `SELECT human_prompt_lease_expires_at > clock_timestamp() FROM agent_sessions WHERE id = $1`, lease.AgentSessionID).Scan(&leaseLive); err != nil {
		return fmt.Errorf("check human prompt lease heartbeat fence: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)`, workflowID).Scan(&active); err != nil {
		return fmt.Errorf("check active Agent Turn during human prompt heartbeat: %w", err)
	}
	if !leaseLive || active {
		return ErrHumanPromptLeaseLost
	}
	result, err := tx.Exec(ctx, `
WITH lease_time AS (SELECT clock_timestamp() AS now)
UPDATE agent_sessions AS session
SET human_prompt_heartbeat_at = lease_time.now,
	    human_prompt_lease_expires_at = lease_time.now + $3 * interval '1 microsecond',
	    updated_at = lease_time.now
FROM lease_time
WHERE session.id = $1 AND session.human_prompt_token = $2`, lease.AgentSessionID, lease.Token, extension.Microseconds())
	if err != nil {
		return fmt.Errorf("heartbeat human prompt lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrHumanPromptLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit human prompt lease heartbeat: %w", err)
	}
	return nil
}

// ReleaseHumanPromptLease clears only the exact durable human prompt fence.
func (store *Store) ReleaseHumanPromptLease(ctx context.Context, lease HumanPromptLease) error {
	if !validUUID(lease.AgentSessionID) || !validUUID(lease.Token) {
		return ErrHumanPromptLeaseLost
	}
	result, err := store.pool.Exec(ctx, `
UPDATE agent_sessions
SET human_prompt_token = NULL, human_prompt_leased_at = NULL,
    human_prompt_lease_expires_at = NULL, human_prompt_heartbeat_at = NULL,
    updated_at = clock_timestamp()
WHERE id = $1 AND human_prompt_token = $2`, lease.AgentSessionID, lease.Token)
	if err != nil {
		return fmt.Errorf("release human prompt lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrHumanPromptLeaseLost
	}
	return nil
}

func lockHumanPromptSession(ctx context.Context, tx pgx.Tx, sessionID string) (string, string, AgentSession, error) {
	var workflowID, assignmentID, lockedWorkflowID, lockedAssignmentID string
	if err := tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, assignment.id::text
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = $1`, sessionID).Scan(&workflowID, &assignmentID); err != nil {
		return "", "", AgentSession{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 FOR UPDATE`, workflowID).Scan(&lockedWorkflowID); err != nil {
		return "", "", AgentSession{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM agent_assignments WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, assignmentID, workflowID).Scan(&lockedAssignmentID); err != nil {
		return "", "", AgentSession{}, err
	}
	session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1 AND agent_assignment_id = $2 FOR UPDATE`, sessionID, assignmentID))
	return workflowID, assignmentID, session, err
}

func existingHumanPromptAdmissionError(ctx context.Context, tx pgx.Tx, sessionID string) error {
	var leaseLive bool
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(human_prompt_lease_expires_at > clock_timestamp(), FALSE)
FROM agent_sessions WHERE id = $1`, sessionID).Scan(&leaseLive); err != nil {
		return fmt.Errorf("classify existing human prompt admission: %w", err)
	}
	if leaseLive {
		return ErrHumanPromptLeaseActive
	}
	return ErrHumanPromptAdmissionUncertain
}

func validatePreparationSpec(spec AgentTurnPreparationSpec) error {
	for role, binding := range map[workflow.Role]AssignmentRuntimeBinding{
		workflow.RoleDeveloper: spec.Developer.Binding,
		workflow.RoleReviewer:  spec.Reviewer.Binding,
	} {
		if strings.TrimSpace(binding.AgentProfileName) == "" || strings.TrimSpace(binding.RuntimeProfileName) == "" ||
			strings.TrimSpace(binding.RuntimeProfileVersion) == "" || !validRuntimeProfileContentSHA256(binding.RuntimeProfileContentSHA256) ||
			strings.TrimSpace(binding.RuntimeImageDigest) == "" {
			return fmt.Errorf("prepare Agent Turn: %s binding is incomplete", role)
		}
	}
	return nil
}

func decodeAgentTurnPreparationPayload(value json.RawMessage) (agentTurnPreparationPayload, error) {
	var payload agentTurnPreparationPayload
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return agentTurnPreparationPayload{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return agentTurnPreparationPayload{}, errors.New("preparation payload has trailing content")
	}
	return payload, nil
}

func validAgentTurnPreparationPayload(payload agentTurnPreparationPayload, job Job) bool {
	if job.WorkflowID == "" || job.WorkflowAttemptID == "" || job.AgentAssignmentID != "" ||
		job.AgentSessionID != "" || job.AgentTurnID != "" || job.ExecutionEpoch != 0 || payload.Revision <= 0 {
		return false
	}
	if payload.Mode != workflow.AssignmentGenerationCurrent && payload.Mode != workflow.AssignmentGenerationRetained && payload.Mode != workflow.AssignmentGenerationNew {
		return false
	}
	if payload.Role != workflow.RoleDeveloper && payload.Role != workflow.RoleReviewer {
		return false
	}
	switch payload.Purpose {
	case workflow.TurnPurposeInitialDevelopment, workflow.TurnPurposeReview,
		workflow.TurnPurposeRequestedChanges, workflow.TurnPurposeRetry,
		workflow.TurnPurposeSynchronization, workflow.TurnPurposeReactivation:
	default:
		return false
	}
	return (payload.RetryOfTurnID == "") == (payload.Purpose != workflow.TurnPurposeRetry) &&
		(payload.RetryOfTurnID == "" || validUUID(payload.RetryOfTurnID))
}

func lockPreparationWorkflow(ctx context.Context, tx pgx.Tx, job Job, payload agentTurnPreparationPayload) (string, error) {
	var status, assignmentStatus, runtimeState string
	var revision int64
	if err := tx.QueryRow(ctx, `
SELECT status, state_revision, desired_assignment_status, desired_runtime_state
FROM workflows WHERE id = $1 FOR UPDATE`, job.WorkflowID).Scan(&status, &revision, &assignmentStatus, &runtimeState); err != nil ||
		revision != payload.Revision || !workflowAllowsTurns(status) || assignmentStatus != "ACTIVE" || runtimeState != "ACTIVE" {
		return "", ErrAgentTurnPreparationFenceLost
	}
	expectedRole := workflow.RoleDeveloper
	if status == string(workflow.StateReviewing) {
		expectedRole = workflow.RoleReviewer
	} else if status != string(workflow.StateDeveloping) {
		return "", ErrAgentTurnPreparationFenceLost
	}
	if payload.Role != expectedRole {
		return "", ErrAgentTurnPreparationFenceLost
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT active FROM workflow_attempts WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, job.WorkflowAttemptID, job.WorkflowID).Scan(&active); err != nil || !active {
		return "", ErrAgentTurnPreparationFenceLost
	}
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active
)`, job.WorkflowID).Scan(&active); err != nil || active {
		return "", ErrAgentTurnPreparationFenceLost
	}
	var proposalID, headSHA string
	err := tx.QueryRow(ctx, `SELECT id::text, head_sha FROM change_proposals WHERE workflow_id = $1 AND active FOR UPDATE`, job.WorkflowID).Scan(&proposalID, &headSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		if payload.ExpectedHeadSHA != "" || payload.Role == workflow.RoleReviewer {
			return "", ErrAgentTurnPreparationFenceLost
		}
		return "", nil
	}
	if err != nil || payload.ExpectedHeadSHA == "" || payload.ExpectedHeadSHA != headSHA {
		return "", ErrAgentTurnPreparationFenceLost
	}
	return proposalID, nil
}

type agentProfileConfig struct {
	Name         string            `json:"name"`
	Path         string            `json:"path"`
	Role         workflow.Role     `json:"role"`
	Runtime      string            `json:"runtime"`
	Model        string            `json:"model"`
	Variant      json.RawMessage   `json:"variant,omitempty"`
	Steps        int               `json:"steps"`
	Permissions  map[string]string `json:"permissions"`
	Instructions string            `json:"instructions"`
}

func validateAgentProfileSnapshot(profile AgentProfileSnapshot, preparation AgentTurnPreparation) (json.RawMessage, error) {
	if strings.TrimSpace(profile.CommitSHA) == "" || len(profile.ContentSHA256) != 32 {
		return nil, errors.New("prepare Agent Turn: invalid Agent Profile provenance")
	}
	return validateAgentProfileConfig(profile.Config, preparation)
}

func validateAgentProfileConfig(value json.RawMessage, preparation AgentTurnPreparation) (json.RawMessage, error) {
	config, err := canonicalJSON(value)
	if err != nil {
		return nil, fmt.Errorf("prepare Agent Turn: Agent Profile config: %w", err)
	}
	var snapshot agentProfileConfig
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("prepare Agent Turn: Agent Profile structure: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("prepare Agent Turn: Agent Profile structure has trailing content")
	}
	if strings.TrimSpace(snapshot.Name) == "" || snapshot.Name != preparation.Assignment.AgentProfileName {
		return nil, errors.New("prepare Agent Turn: Agent Profile name does not match Assignment")
	}
	expectedPath := ".omnigrex/team/developer.md"
	if preparation.Role == workflow.RoleReviewer {
		expectedPath = ".omnigrex/team/reviewer.md"
	}
	if snapshot.Path != expectedPath {
		return nil, errors.New("prepare Agent Turn: Agent Profile path does not match Role")
	}
	if snapshot.Role != workflow.RoleDeveloper && snapshot.Role != workflow.RoleReviewer || snapshot.Role != preparation.Role {
		return nil, errors.New("prepare Agent Turn: Agent Profile Role does not match preparation")
	}
	runtimeParts := strings.Split(snapshot.Runtime, "/")
	if len(runtimeParts) != 2 || runtimeParts[0] == "" || runtimeParts[1] == "" || containsWhitespaceOrControl(snapshot.Runtime) ||
		runtimeParts[0] != preparation.Assignment.RuntimeProfileName || runtimeParts[1] != preparation.Assignment.RuntimeProfileVersion {
		return nil, errors.New("prepare Agent Turn: Agent Profile runtime does not match Assignment")
	}
	if !validProfileReference(snapshot.Model) {
		return nil, errors.New("prepare Agent Turn: Agent Profile model must use provider/model syntax")
	}
	if snapshot.Steps <= 0 {
		return nil, errors.New("prepare Agent Turn: Agent Profile steps must be positive")
	}
	if len(snapshot.Permissions) == 0 {
		return nil, errors.New("prepare Agent Turn: Agent Profile permissions must be a nonempty object")
	}
	for tool, action := range snapshot.Permissions {
		if strings.TrimSpace(tool) == "" || strings.TrimSpace(action) == "" {
			return nil, errors.New("prepare Agent Turn: Agent Profile permissions contain a blank key or value")
		}
	}
	if strings.TrimSpace(snapshot.Instructions) == "" {
		return nil, errors.New("prepare Agent Turn: Agent Profile instructions are blank")
	}
	if snapshot.Variant != nil {
		var variant string
		if err := json.Unmarshal(snapshot.Variant, &variant); err != nil || strings.TrimSpace(variant) == "" || containsWhitespaceOrControl(variant) {
			return nil, errors.New("prepare Agent Turn: Agent Profile variant is invalid")
		}
	}
	var decoded any
	if err := json.Unmarshal(config, &decoded); err != nil {
		return nil, err
	}
	if containsCredential(decoded) {
		return nil, errors.New("prepare Agent Turn: Agent Profile config contains credentials")
	}
	return config, nil
}

func validProfileReference(value string) bool {
	if strings.Count(value, "/") != 1 || containsWhitespaceOrControl(value) {
		return false
	}
	parts := strings.Split(value, "/")
	return parts[0] != "" && parts[1] != ""
}

func containsWhitespaceOrControl(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsCredential(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if normalized == "authorization" || normalized == "api_key" || normalized == "private_key" ||
				normalized == "password" || normalized == "secret" || normalized == "credential" || normalized == "credentials" ||
				normalized == "token" || strings.HasSuffix(normalized, "_password") || strings.HasSuffix(normalized, "_secret") ||
				strings.HasSuffix(normalized, "_credential") || strings.HasSuffix(normalized, "_credentials") || strings.HasSuffix(normalized, "_token") {
				return true
			}
			if containsCredential(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsCredential(child) {
				return true
			}
		}
	}
	return false
}

func ensurePreparationAssignments(ctx context.Context, tx pgx.Tx, preparationJobID, workflowID string, mode workflow.AssignmentGeneration, spec AgentTurnPreparationSpec) (map[workflow.Role]AgentAssignment, error) {
	rows, err := tx.Query(ctx, agentAssignmentSelect+`
 WHERE workflow_id = $1 AND status <> 'SUPERSEDED' AND state_deleted_at IS NULL
 ORDER BY role FOR UPDATE`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("lock current Agent Assignments: %w", err)
	}
	current := make(map[workflow.Role]AgentAssignment, 2)
	for rows.Next() {
		assignment, err := scanAgentAssignment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		current[assignment.Role] = assignment
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	bindings := map[workflow.Role]AssignmentRuntimeBinding{
		workflow.RoleDeveloper: spec.Developer.Binding,
		workflow.RoleReviewer:  spec.Reviewer.Binding,
	}
	if mode == workflow.AssignmentGenerationNew {
		if len(current) == 2 {
			generation := 0
			for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
				assignment, ok := current[role]
				if !ok || assignment.Status != AgentAssignmentActive || assignment.createdByPreparationJobID != preparationJobID || generation != 0 && assignment.Generation != generation {
					return nil, ErrAgentTurnPreparationFenceLost
				}
				if assignment.AssignmentRuntimeBinding != bindings[role] {
					return nil, ErrAssignmentConfigurationConflict
				}
				generation = assignment.Generation
			}
			return current, nil
		}
		if len(current) != 0 {
			return nil, ErrAgentTurnPreparationFenceLost
		}
		if _, err := tx.Exec(ctx, `
UPDATE agent_assignments SET status = 'SUPERSEDED', updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status <> 'SUPERSEDED'`, workflowID); err != nil {
			return nil, fmt.Errorf("supersede collected Agent Assignments: %w", err)
		}
		var generation int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM agent_assignments WHERE workflow_id = $1`, workflowID).Scan(&generation); err != nil {
			return nil, err
		}
		for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
			id, err := randomUUID()
			if err != nil {
				return nil, err
			}
			binding := bindings[role]
			path := "assignment-" + id + "/runtime-state"
			if _, err := tx.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, generation, status, agent_profile_name,
    runtime_profile_name, runtime_profile_version, runtime_profile_content_sha256,
    runtime_image_digest, runtime_state_path, created_by_preparation_job_id
)
VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $7, $8, $9, $10, $11)`, id, workflowID, role, generation,
				binding.AgentProfileName, binding.RuntimeProfileName, binding.RuntimeProfileVersion,
				binding.RuntimeProfileContentSHA256, binding.RuntimeImageDigest, path, preparationJobID); err != nil {
				return nil, fmt.Errorf("create %s Agent Assignment: %w", role, err)
			}
			assignment, err := scanAgentAssignment(tx.QueryRow(ctx, agentAssignmentSelect+` WHERE id = $1`, id))
			if err != nil {
				return nil, err
			}
			current[role] = assignment
		}
	} else {
		if len(current) != 2 {
			return nil, ErrAgentTurnPreparationFenceLost
		}
		for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
			assignment, ok := current[role]
			if !ok {
				return nil, ErrAgentTurnPreparationFenceLost
			}
			if assignment.AssignmentRuntimeBinding != bindings[role] {
				return nil, ErrAssignmentConfigurationConflict
			}
			if mode == workflow.AssignmentGenerationRetained {
				if assignment.Status == AgentAssignmentActive && assignment.reactivatedByPreparationJobID != preparationJobID ||
					assignment.Status != AgentAssignmentCompleted && assignment.Status != AgentAssignmentActive {
					return nil, ErrAgentTurnPreparationFenceLost
				}
				if assignment.Status == AgentAssignmentCompleted {
					if _, err := tx.Exec(ctx, `
UPDATE agent_assignments SET status = 'ACTIVE', completed_at = NULL, retention_until = NULL,
	    reactivated_by_preparation_job_id = $2, updated_at = clock_timestamp()
WHERE id = $1`, assignment.ID, preparationJobID); err != nil {
						return nil, err
					}
				}
				assignment.Status, assignment.CompletedAt, assignment.RetentionUntil = AgentAssignmentActive, nil, nil
				assignment.reactivatedByPreparationJobID = preparationJobID
				current[role] = assignment
			}
		}
		if mode == workflow.AssignmentGenerationCurrent {
			allActive, allWaiting := true, true
			for _, assignment := range current {
				allActive = allActive && assignment.Status == AgentAssignmentActive
				allWaiting = allWaiting && assignment.Status == AgentAssignmentWaitingForHuman
			}
			if !allActive {
				if !allWaiting {
					return nil, ErrAgentTurnPreparationFenceLost
				}
				allowed, err := preparationHandoffAllowsWaitingAssignments(ctx, tx, preparationJobID, workflowID, false)
				if err != nil {
					return nil, err
				}
				if !allowed {
					return nil, ErrAgentTurnPreparationFenceLost
				}
				updated, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET status = 'ACTIVE', completed_at = NULL, retention_until = NULL,
    reactivated_by_preparation_job_id = $2, updated_at = clock_timestamp()
WHERE workflow_id = $1 AND status = 'WAITING_FOR_HUMAN'
  AND state_deleted_at IS NULL`, workflowID, preparationJobID)
				if err != nil {
					return nil, fmt.Errorf("reactivate preparation handoff Assignments: %w", err)
				}
				if updated.RowsAffected() != 2 {
					return nil, ErrAgentTurnPreparationFenceLost
				}
				for role, assignment := range current {
					assignment.Status = AgentAssignmentActive
					assignment.CompletedAt, assignment.RetentionUntil = nil, nil
					assignment.reactivatedByPreparationJobID = preparationJobID
					current[role] = assignment
				}
			}
		}
	}
	return current, nil
}

func preparationHandoffAllowsWaitingAssignments(ctx context.Context, tx pgx.Tx, preparationJobID, workflowID string, configurationConflictOnly bool) (bool, error) {
	var allowed bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM jobs AS preparation
    JOIN workflow_attempts AS handoff_attempt
      ON handoff_attempt.id = preparation.workflow_attempt_id
     AND handoff_attempt.workflow_id = preparation.workflow_id
    JOIN workflow_attempts AS current_attempt
      ON current_attempt.workflow_id = handoff_attempt.workflow_id
     AND current_attempt.attempt_number = handoff_attempt.attempt_number + 1
    WHERE current_attempt.id = (SELECT workflow_attempt_id FROM jobs WHERE id = $1)
      AND current_attempt.workflow_id = $2 AND current_attempt.active
      AND NOT handoff_attempt.active
      AND preparation.kind = 'PREPARE_AGENT_TURN'
      AND (
          NOT $3 AND handoff_attempt.human_handoff_reason = $4 AND preparation.status = 'FAILED'
          OR handoff_attempt.human_handoff_reason = $5 AND preparation.status = 'SUCCEEDED'
             AND preparation.result->>'reason' = $5
      )
)`, preparationJobID, workflowID, configurationConflictOnly, workflow.ReasonAgentTurnPreparationFailed,
		workflow.ReasonAssignmentConfigurationConflict).Scan(&allowed); err != nil {
		return false, fmt.Errorf("verify preparation handoff waiting Assignments: %w", err)
	}
	return allowed, nil
}

func revalidateAssignmentConfigurationConflict(ctx context.Context, tx pgx.Tx, job Job, payload agentTurnPreparationPayload, spec AgentTurnPreparationSpec) (bool, error) {
	rows, err := tx.Query(ctx, agentAssignmentSelect+`
 WHERE workflow_id = $1 AND status <> 'SUPERSEDED' AND state_deleted_at IS NULL
 ORDER BY role FOR UPDATE`, job.WorkflowID)
	if err != nil {
		return false, fmt.Errorf("lock Assignments for configuration conflict: %w", err)
	}
	current := make(map[workflow.Role]AgentAssignment, 2)
	for rows.Next() {
		assignment, err := scanAgentAssignment(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		current[assignment.Role] = assignment
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()

	bindings := map[workflow.Role]AssignmentRuntimeBinding{
		workflow.RoleDeveloper: spec.Developer.Binding,
		workflow.RoleReviewer:  spec.Reviewer.Binding,
	}
	conflict := false
	generation := 0
	if payload.Mode == workflow.AssignmentGenerationNew {
		if len(current) == 0 {
			return false, nil
		}
		if len(current) != 2 {
			return false, ErrAgentTurnPreparationFenceLost
		}
		for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
			assignment, ok := current[role]
			if !ok || assignment.Status != AgentAssignmentActive || assignment.createdByPreparationJobID != job.ID || generation != 0 && assignment.Generation != generation {
				return false, ErrAgentTurnPreparationFenceLost
			}
			if assignment.AssignmentRuntimeBinding != bindings[role] {
				conflict = true
			}
			generation = assignment.Generation
		}
	} else {
		if len(current) != 2 {
			return false, ErrAgentTurnPreparationFenceLost
		}
		allActive, allWaiting := true, true
		for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
			assignment, ok := current[role]
			if !ok || generation != 0 && assignment.Generation != generation {
				return false, ErrAgentTurnPreparationFenceLost
			}
			if payload.Mode == workflow.AssignmentGenerationCurrent {
				allActive = allActive && assignment.Status == AgentAssignmentActive
				allWaiting = allWaiting && assignment.Status == AgentAssignmentWaitingForHuman
			}
			if payload.Mode == workflow.AssignmentGenerationRetained &&
				(assignment.Status == AgentAssignmentActive && assignment.reactivatedByPreparationJobID != job.ID ||
					assignment.Status != AgentAssignmentCompleted && assignment.Status != AgentAssignmentActive) {
				return false, ErrAgentTurnPreparationFenceLost
			}
			if assignment.AssignmentRuntimeBinding != bindings[role] {
				conflict = true
			}
			generation = assignment.Generation
		}
		if payload.Mode == workflow.AssignmentGenerationCurrent && !allActive {
			if !allWaiting {
				return false, ErrAgentTurnPreparationFenceLost
			}
			allowed, err := preparationHandoffAllowsWaitingAssignments(ctx, tx, job.ID, job.WorkflowID, true)
			if err != nil {
				return false, err
			}
			if !allowed {
				return false, ErrAgentTurnPreparationFenceLost
			}
		}
	}
	if conflict {
		return true, nil
	}

	assignment := current[payload.Role]
	session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+`
 WHERE agent_assignment_id = $1 AND status IN ('CREATING', 'ACTIVE', 'RETAINED') FOR UPDATE`, assignment.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return session.RuntimeProfileName != assignment.RuntimeProfileName ||
		session.RuntimeProfileVersion != assignment.RuntimeProfileVersion ||
		session.RuntimeProfileContentSHA256 != assignment.RuntimeProfileContentSHA256 ||
		session.RuntimeImageDigest != assignment.RuntimeImageDigest ||
		session.RuntimeStatePath != assignment.RuntimeStatePath, nil
}

func ensurePreparationSession(ctx context.Context, tx pgx.Tx, assignment AgentAssignment) (AgentSession, error) {
	session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+`
 WHERE agent_assignment_id = $1 AND status IN ('CREATING', 'ACTIVE', 'RETAINED') FOR UPDATE`, assignment.ID))
	if err == nil {
		if session.RuntimeProfileName != assignment.RuntimeProfileName ||
			session.RuntimeProfileVersion != assignment.RuntimeProfileVersion ||
			session.RuntimeProfileContentSHA256 != assignment.RuntimeProfileContentSHA256 ||
			session.RuntimeImageDigest != assignment.RuntimeImageDigest ||
			session.RuntimeStatePath != assignment.RuntimeStatePath {
			return AgentSession{}, ErrAssignmentConfigurationConflict
		}
		return session, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentSession{}, err
	}
	id, err := randomUUID()
	if err != nil {
		return AgentSession{}, err
	}
	var number int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(session_number), 0) + 1 FROM agent_sessions WHERE agent_assignment_id = $1`, assignment.ID).Scan(&number); err != nil {
		return AgentSession{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, runtime_profile_name, runtime_profile_version,
    runtime_profile_content_sha256, runtime_image_digest, runtime_state_path, status,
    control_owner, control_revision, controller_id, control_acquired_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'CREATING', 'AUTOMATION', 1, 'automation', clock_timestamp())`,
		id, assignment.ID, number, assignment.RuntimeProfileName, assignment.RuntimeProfileVersion,
		assignment.RuntimeProfileContentSHA256, assignment.RuntimeImageDigest, assignment.RuntimeStatePath); err != nil {
		return AgentSession{}, fmt.Errorf("create Agent Session: %w", err)
	}
	return scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, id))
}

const agentAssignmentSelect = `
SELECT id::text, COALESCE(created_by_preparation_job_id::text, ''),
       COALESCE(reactivated_by_preparation_job_id::text, ''), workflow_id::text, role, generation, status, agent_profile_name,
       runtime_profile_name, runtime_profile_version, COALESCE(runtime_profile_content_sha256, ''), runtime_image_digest,
       runtime_state_path, created_at, updated_at, completed_at, retention_until, state_deleted_at
FROM agent_assignments`

func scanAgentAssignment(row rowScanner) (AgentAssignment, error) {
	var assignment AgentAssignment
	err := row.Scan(&assignment.ID, &assignment.createdByPreparationJobID, &assignment.reactivatedByPreparationJobID,
		&assignment.WorkflowID, &assignment.Role, &assignment.Generation,
		&assignment.Status, &assignment.AgentProfileName, &assignment.RuntimeProfileName,
		&assignment.RuntimeProfileVersion, &assignment.RuntimeProfileContentSHA256,
		&assignment.RuntimeImageDigest, &assignment.RuntimeStatePath,
		&assignment.CreatedAt, &assignment.UpdatedAt, &assignment.CompletedAt,
		&assignment.RetentionUntil, &assignment.StateDeletedAt)
	return assignment, err
}

const agentSessionSelect = `
SELECT id::text, agent_assignment_id::text, session_number, COALESCE(acp_session_id, ''),
       runtime_profile_name, runtime_profile_version, COALESCE(runtime_profile_content_sha256, ''),
       runtime_image_digest, runtime_state_path,
	       capabilities, status, control_owner, control_revision, COALESCE(controller_id, ''),
	       control_acquired_at, COALESCE(human_prompt_token::text, ''), human_prompt_leased_at,
	       human_prompt_lease_expires_at, human_prompt_heartbeat_at,
	       next_turn_number, next_execution_epoch, created_at, updated_at,
       activated_at, retained_at, state_deleted_at
FROM agent_sessions`

func scanAgentSession(row rowScanner) (AgentSession, error) {
	var session AgentSession
	var capabilities []byte
	err := row.Scan(&session.ID, &session.AgentAssignmentID, &session.SessionNumber,
		&session.ACPSessionID, &session.RuntimeProfileName, &session.RuntimeProfileVersion,
		&session.RuntimeProfileContentSHA256, &session.RuntimeImageDigest,
		&session.RuntimeStatePath, &capabilities, &session.Status,
		&session.ControlOwner, &session.ControlRevision, &session.ControllerID,
		&session.ControlAcquiredAt, &session.HumanPromptToken, &session.HumanPromptLeasedAt,
		&session.HumanPromptLeaseExpiresAt, &session.HumanPromptHeartbeatAt,
		&session.NextTurnNumber, &session.NextExecutionEpoch,
		&session.CreatedAt, &session.UpdatedAt, &session.ActivatedAt, &session.RetainedAt,
		&session.StateDeletedAt)
	if err != nil {
		return AgentSession{}, err
	}
	session.Capabilities, err = canonicalJSON(capabilities)
	return session, err
}

func validRuntimeProfileContentSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
