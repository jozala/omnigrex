package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	ErrAgentSessionNotFound        = errors.New("agent session not found")
	ErrAgentTurnActive             = errors.New("agent turn already active")
	ErrAgentTurnFenceLost          = errors.New("agent turn fence lost")
	ErrAgentTurnConcurrencyLimit   = errors.New("agent turn concurrency limit reached")
	ErrAgentTurnMutationsUnsettled = errors.New("agent turn mutations are unsettled")
	ErrAgentTurnHierarchyInactive  = errors.New("agent turn hierarchy is inactive")
	ErrAgentTurnNotExpired         = errors.New("agent turn ownership is not expired")
	ErrAgentTurnRecoveryUnsettled  = errors.New("agent turn recovery is unsettled")
	ErrAgentTurnRecoveryFenceLost  = errors.New("agent turn recovery job fence lost")
	ErrMutationAdmissionClosed     = errors.New("mutation admission is closed")
	ErrMutationOperationConflict   = errors.New("mutation operation identity conflict")
	ErrMutationStateConflict       = errors.New("mutation state conflict")
)

const (
	AgentTurnQueue                     = "agent-turns"
	RunAgentTurnJobKind                = "RUN_AGENT_TURN"
	AgentTurnRecoveryQueue             = "agent-turn-recovery"
	StopStaleRuntimeJobKind            = "STOP_STALE_RUNTIME"
	ReconcileAgentTurnMutationsJobKind = "RECONCILE_AGENT_TURN_MUTATIONS"
	agentTurnRecoveryJobMaxAttempts    = 3
	stopStaleRuntimeJobPriority        = 100
	reconcileTurnMutationsJobPriority  = 90

	RuntimeLabelAssignmentID = "omnigrex.agent_assignment_id"
	RuntimeLabelSessionID    = "omnigrex.agent_session_id"
	RuntimeLabelTurnID       = "omnigrex.agent_turn_id"
	RuntimeLabelEpoch        = "omnigrex.execution_epoch"

	agentTurnSlotsLockID = int64(0x4f4d4e49534c4f54)
)

// AgentTurnStatus is the durable lifecycle state of an Agent Turn.
type AgentTurnStatus string

const (
	AgentTurnQueued      AgentTurnStatus = "QUEUED"
	AgentTurnStarting    AgentTurnStatus = "STARTING"
	AgentTurnRunning     AgentTurnStatus = "RUNNING"
	AgentTurnCancelling  AgentTurnStatus = "CANCELLING"
	AgentTurnSettling    AgentTurnStatus = "SETTLING"
	AgentTurnReconciling AgentTurnStatus = "RECONCILING"
	AgentTurnSucceeded   AgentTurnStatus = "SUCCEEDED"
	AgentTurnFailed      AgentTurnStatus = "FAILED"
	AgentTurnInterrupted AgentTurnStatus = "INTERRUPTED"
	AgentTurnTimedOut    AgentTurnStatus = "TIMED_OUT"
)

// AgentTurnSpec captures the immutable inputs used to allocate an Agent Turn.
type AgentTurnSpec struct {
	AgentSessionID            string
	WorkflowAttemptID         string
	RetryOfTurnID             string
	Purpose                   workflow.TurnPurpose
	ChangeProposalID          string
	ExpectedHeadSHA           string
	ControlRevision           int64
	AgentProfileCommitSHA     string
	AgentProfileContentSHA256 []byte
	AgentProfileConfig        json.RawMessage
}

// AgentTurn identifies one prompt-response interaction and its execution fence.
type AgentTurn struct {
	AgentTurnSpec
	ID                    string
	AgentAssignmentID     string
	TurnNumber            int64
	ExecutionEpoch        int64
	Status                AgentTurnStatus
	MutationAdmissionOpen bool
	CreatedAt             time.Time
}

// AgentTurnLease binds Runtime Process ownership to the durable execution job attempt.
type AgentTurnLease struct {
	AgentTurn
	JobLease       JobLease
	OwnerID        string
	OwnerToken     string
	LeaseExpiresAt time.Time
}

// AgentTurnCompletion describes the guarded terminal state of an Agent Turn.
type AgentTurnCompletion struct {
	Status    AgentTurnStatus
	Outcome   json.RawMessage
	LastError string
}

// AgentTurnRecovery is the durable result of fencing expired execution ownership.
type AgentTurnRecovery struct {
	TurnID                  string
	JobID                   string
	ExecutionEpoch          int64
	Status                  AgentTurnStatus
	MutationsUnsettled      bool
	SuccessorAllowed        bool
	RuntimeStopRequired     bool
	RecoveryStartedAt       *time.Time
	RuntimeStoppedAt        *time.Time
	RecoverySettledAt       *time.Time
	StopRuntimeJobID        string
	ReconcileMutationsJobID string
}

// RecoveredMutationOutcome is a known terminal result established by reconciliation.
type RecoveredMutationOutcome struct {
	State     MutationState
	Result    json.RawMessage
	LastError string
}

// MutationState is the durable state of an admitted external mutation.
type MutationState string

const (
	MutationReserved    MutationState = "RESERVED"
	MutationInFlight    MutationState = "IN_FLIGHT"
	MutationUnknown     MutationState = "UNKNOWN"
	MutationSucceeded   MutationState = "SUCCEEDED"
	MutationFailed      MutationState = "FAILED"
	MutationReconciling MutationState = "RECONCILING"
)

// MutationSpec is the stable definition reserved before an external side effect.
type MutationSpec struct {
	OperationID        string
	ToolName           string
	Request            json.RawMessage
	ExternalService    string
	ExternalResourceID string
	ExpectedSHA        string
}

// MutationReservation records admission, sequencing, and current durable state.
type MutationReservation struct {
	ID                 string
	AgentTurnID        string
	ExecutionEpoch     int64
	InvocationNumber   int64
	OperationID        string
	ToolName           string
	Request            json.RawMessage
	State              MutationState
	ExternalService    string
	ExternalResourceID string
	ExpectedSHA        string
	Result             json.RawMessage
	LastError          string
	AdmittedAt         time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
}

// AllocateAgentTurn locks the active hierarchy and allocates monotonic turn and epoch identities.
func (store *Store) AllocateAgentTurn(ctx context.Context, spec AgentTurnSpec) (AgentTurn, error) {
	config, err := validateAgentTurnSpec(spec)
	if err != nil {
		return AgentTurn{}, err
	}
	spec.AgentProfileConfig = config
	turnID, err := randomUUID()
	if err != nil {
		return AgentTurn{}, fmt.Errorf("allocate agent turn: %w", err)
	}
	jobID, err := randomUUID()
	if err != nil {
		return AgentTurn{}, fmt.Errorf("allocate agent turn job: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurn{}, fmt.Errorf("begin agent turn allocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	hierarchy, err := lookupSessionHierarchy(ctx, tx, spec.AgentSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurn{}, ErrAgentSessionNotFound
	}
	if err != nil {
		return AgentTurn{}, fmt.Errorf("look up agent turn hierarchy: %w", err)
	}
	locked, err := lockHierarchy(ctx, tx, hierarchy, spec.WorkflowAttemptID, true)
	if err != nil {
		return AgentTurn{}, err
	}
	if locked.controlRevision != spec.ControlRevision {
		return AgentTurn{}, ErrAgentTurnFenceLost
	}
	assignment, err := scanAgentAssignment(tx.QueryRow(ctx, agentAssignmentSelect+` WHERE id = $1 FOR UPDATE`, hierarchy.assignmentID))
	if err != nil {
		return AgentTurn{}, fmt.Errorf("read Agent Turn Assignment: %w", err)
	}
	profileConfig, err := validateAgentProfileConfig(spec.AgentProfileConfig, AgentTurnPreparation{
		Role: assignment.Role, Assignment: assignment,
	})
	if err != nil {
		return AgentTurn{}, fmt.Errorf("allocate Agent Turn: %w", err)
	}
	spec.AgentProfileConfig = profileConfig
	var active, unsettled bool
	var recoveryUnsettled bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM agent_turns WHERE agent_session_id = $1
AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL)`, hierarchy.sessionID).Scan(&recoveryUnsettled); err != nil {
		return AgentTurn{}, fmt.Errorf("check Agent Turn recovery barrier: %w", err)
	}
	if recoveryUnsettled {
		return AgentTurn{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active)`, hierarchy.workflowID).Scan(&active); err != nil {
		return AgentTurn{}, fmt.Errorf("check active agent turn: %w", err)
	}
	if active {
		return AgentTurn{}, ErrAgentTurnActive
	}
	var livePreparation bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM jobs
    WHERE workflow_id = $1 AND kind = $2 AND status IN ('AVAILABLE', 'LEASED')
)`, hierarchy.workflowID, PrepareAgentTurnJobKind).Scan(&livePreparation); err != nil {
		return AgentTurn{}, fmt.Errorf("check live Agent Turn preparation: %w", err)
	}
	if livePreparation {
		return AgentTurn{}, ErrWorkflowSuccessorConflict
	}
	var proposalID, headSHA string
	err = tx.QueryRow(ctx, `
SELECT id::text, head_sha FROM change_proposals
WHERE workflow_id = $1 AND active FOR UPDATE`, hierarchy.workflowID).Scan(&proposalID, &headSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		if spec.ChangeProposalID != "" {
			return AgentTurn{}, ErrAgentTurnFenceLost
		}
	} else if err != nil {
		return AgentTurn{}, fmt.Errorf("lock Agent Turn Change Proposal: %w", err)
	} else if spec.ChangeProposalID != proposalID || spec.ExpectedHeadSHA != headSHA {
		return AgentTurn{}, ErrAgentTurnFenceLost
	}
	if err := tx.QueryRow(ctx, unsettledMutationsForSessionSQL, hierarchy.sessionID).Scan(&unsettled); err != nil {
		return AgentTurn{}, fmt.Errorf("check prior agent turn mutations: %w", err)
	}
	if unsettled {
		return AgentTurn{}, ErrAgentTurnMutationsUnsettled
	}
	if spec.RetryOfTurnID != "" {
		var retrySession, retryStatus string
		var retryActive, retryRecoverySettled bool
		err := tx.QueryRow(ctx, `
SELECT agent_session_id::text, status, active, recovery_settled_at IS NOT NULL
FROM agent_turns WHERE id = $1 FOR UPDATE`, spec.RetryOfTurnID).Scan(&retrySession, &retryStatus, &retryActive, &retryRecoverySettled)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (retrySession != hierarchy.sessionID || retryActive || !retryableTurnStatus(AgentTurnStatus(retryStatus), retryRecoverySettled)) {
			return AgentTurn{}, errors.New("allocate agent turn: retry target is not an inactive retryable turn in this session")
		}
		if err != nil {
			return AgentTurn{}, fmt.Errorf("lock retry target: %w", err)
		}
	}

	var createdAt time.Time
	err = tx.QueryRow(ctx, `
	INSERT INTO agent_turns (
	    id, workflow_id, agent_session_id, workflow_attempt_id, turn_number, execution_epoch,
	    retry_of_turn_id, status, active, control_revision, agent_profile_commit_sha,
	    agent_profile_content_sha256, agent_profile_config, purpose, change_proposal_id, expected_head_sha
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, 'QUEUED', TRUE, $8, $9, $10, $11, $12, $13, $14)
	RETURNING created_at`, turnID, hierarchy.workflowID, hierarchy.sessionID, spec.WorkflowAttemptID, locked.nextTurnNumber,
		locked.nextExecutionEpoch, nullableString(spec.RetryOfTurnID), spec.ControlRevision,
		spec.AgentProfileCommitSHA, spec.AgentProfileContentSHA256, spec.AgentProfileConfig,
		spec.Purpose, nullableString(spec.ChangeProposalID), nullableString(spec.ExpectedHeadSHA)).Scan(&createdAt)
	if err != nil {
		return AgentTurn{}, fmt.Errorf("insert agent turn: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE agent_sessions
SET next_turn_number = next_turn_number + 1,
    next_execution_epoch = next_execution_epoch + 1,
    updated_at = clock_timestamp()
WHERE id = $1`, hierarchy.sessionID); err != nil {
		return AgentTurn{}, fmt.Errorf("advance agent turn counters: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"agent_turn_id": turnID, "agent_session_id": hierarchy.sessionID,
		"execution_epoch": locked.nextExecutionEpoch, "control_revision": spec.ControlRevision,
	})
	if err != nil {
		return AgentTurn{}, fmt.Errorf("encode agent turn job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, clock_timestamp(), 1,
        $5, $6, $7, $8, $9, $10, $11)`, jobID, AgentTurnQueue, RunAgentTurnJobKind,
		payload, "run-agent-turn:"+turnID, hierarchy.workflowID, spec.WorkflowAttemptID,
		hierarchy.assignmentID, hierarchy.sessionID, turnID, locked.nextExecutionEpoch); err != nil {
		return AgentTurn{}, fmt.Errorf("enqueue agent turn job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurn{}, fmt.Errorf("commit agent turn allocation: %w", err)
	}
	return AgentTurn{
		AgentTurnSpec: spec, ID: turnID, AgentAssignmentID: hierarchy.assignmentID,
		TurnNumber: locked.nextTurnNumber, ExecutionEpoch: locked.nextExecutionEpoch,
		Status: AgentTurnQueued, CreatedAt: createdAt,
	}, nil
}

// AcquireAgentTurn binds an unowned queued turn to its live single-attempt execution job.
func (store *Store) AcquireAgentTurn(ctx context.Context, jobLease JobLease, controlRevision int64, ownerID string, lease time.Duration, concurrencyLimit int) (AgentTurnLease, error) {
	if strings.TrimSpace(ownerID) == "" {
		return AgentTurnLease{}, errors.New("acquire agent turn: owner is empty")
	}
	if err := validatePositiveDuration("acquire agent turn lease", lease); err != nil {
		return AgentTurnLease{}, err
	}
	if concurrencyLimit <= 0 || concurrencyLimit > 10_000 {
		return AgentTurnLease{}, errors.New("acquire agent turn: concurrency limit must be between 1 and 10000")
	}
	ownerToken, err := randomUUID()
	if err != nil {
		return AgentTurnLease{}, fmt.Errorf("acquire agent turn: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnLease{}, fmt.Errorf("begin agent turn acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := lockAgentTurnJob(ctx, tx, jobLease, true)
	if err != nil {
		return AgentTurnLease{}, err
	}
	hierarchy := hierarchyFromJob(job)
	locked, err := lockHierarchy(ctx, tx, hierarchy, job.WorkflowAttemptID, false)
	if err != nil {
		return AgentTurnLease{}, err
	}
	if controlRevision != locked.controlRevision {
		return AgentTurnLease{}, ErrAgentTurnFenceLost
	}
	turn, err := lockAgentTurn(ctx, tx, job.AgentTurnID)
	if err != nil {
		return AgentTurnLease{}, err
	}
	if !locked.allowsAcquisition(turn.AgentTurn) {
		return AgentTurnLease{}, ErrAgentTurnHierarchyInactive
	}
	if !turn.active || turn.AgentSessionID != job.AgentSessionID || turn.ExecutionEpoch != job.ExecutionEpoch ||
		turn.ControlRevision != controlRevision || turn.Status != AgentTurnQueued && turn.Status != AgentTurnStarting ||
		turn.ownerID != "" || turn.ownerToken != "" || turn.leasePresent {
		return AgentTurnLease{}, ErrAgentTurnFenceLost
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnLease{}, err
	}
	var occupied int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_turn_slots WHERE lease_expires_at > clock_timestamp()`).Scan(&occupied); err != nil {
		return AgentTurnLease{}, fmt.Errorf("count agent turn slots: %w", err)
	}
	if occupied >= concurrencyLimit {
		return AgentTurnLease{}, ErrAgentTurnConcurrencyLimit
	}
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + $1 * interval '1 microsecond'`, lease.Microseconds()).Scan(&expiresAt); err != nil {
		return AgentTurnLease{}, fmt.Errorf("calculate agent turn lease: %w", err)
	}
	jobUpdate, err := tx.Exec(ctx, `
UPDATE jobs SET lease_expires_at = $4, heartbeat_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, expiresAt)
	if err != nil {
		return AgentTurnLease{}, fmt.Errorf("bind agent turn job lease: %w", err)
	}
	if jobUpdate.RowsAffected() != 1 {
		return AgentTurnLease{}, ErrAgentTurnFenceLost
	}
	attemptUpdate, err := tx.Exec(ctx, `
UPDATE job_attempts SET lease_expires_at = $4, heartbeat_at = clock_timestamp()
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, expiresAt)
	if err != nil {
		return AgentTurnLease{}, fmt.Errorf("bind agent turn job attempt lease: %w", err)
	}
	if attemptUpdate.RowsAffected() != 1 {
		return AgentTurnLease{}, ErrAgentTurnFenceLost
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_turn_slots (
    agent_turn_id, agent_session_id, execution_epoch, control_revision,
    owner_id, owner_token, lease_expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, turn.ID, turn.AgentSessionID, turn.ExecutionEpoch,
		turn.ControlRevision, ownerID, ownerToken, expiresAt); err != nil {
		return AgentTurnLease{}, fmt.Errorf("occupy agent turn slot: %w", err)
	}
	result, err := tx.Exec(ctx, `
UPDATE agent_turns
SET status = 'RUNNING', owner_id = $2, owner_token = $3, leased_at = clock_timestamp(),
    lease_expires_at = $4, heartbeat_at = NULL, mutation_admission_open = FALSE,
    mutation_admission_closed_at = NULL, started_at = COALESCE(started_at, clock_timestamp())
WHERE id = $1 AND status IN ('QUEUED', 'STARTING') AND owner_id IS NULL AND owner_token IS NULL`,
		turn.ID, ownerID, ownerToken, expiresAt)
	if err != nil || result.RowsAffected() != 1 {
		if err == nil {
			err = ErrAgentTurnFenceLost
		}
		return AgentTurnLease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnLease{}, fmt.Errorf("commit agent turn acquisition: %w", err)
	}
	turn.AgentTurn.Status = AgentTurnRunning
	turn.AgentTurn.MutationAdmissionOpen = false
	turn.AgentTurn.AgentAssignmentID = job.AgentAssignmentID
	return AgentTurnLease{
		AgentTurn: turn.AgentTurn, JobLease: jobLease, OwnerID: ownerID,
		OwnerToken: ownerToken, LeaseExpiresAt: expiresAt,
	}, nil
}

// HeartbeatAgentTurn atomically extends job, attempt, turn, and global-slot leases.
func (store *Store) HeartbeatAgentTurn(ctx context.Context, lease AgentTurnLease, extension time.Duration) error {
	if err := validatePositiveDuration("heartbeat agent turn lease", extension); err != nil {
		return err
	}
	return store.withLockedAgentTurnLease(ctx, lease, "heartbeat agent turn", func(tx pgx.Tx, _ lockedTurn) error {
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + $1 * interval '1 microsecond'`, extension.Microseconds()).Scan(&expiresAt); err != nil {
			return err
		}
		updates := []struct {
			query string
			args  []any
		}{
			{`UPDATE jobs SET heartbeat_at = clock_timestamp(), lease_expires_at = $4, updated_at = clock_timestamp() WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`, []any{lease.JobLease.ID, lease.JobLease.LeaseToken, lease.JobLease.Attempt, expiresAt}},
			{`UPDATE job_attempts SET heartbeat_at = clock_timestamp(), lease_expires_at = $4 WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`, []any{lease.JobLease.ID, lease.JobLease.LeaseToken, lease.JobLease.Attempt, expiresAt}},
			{`UPDATE agent_turns SET heartbeat_at = clock_timestamp(), lease_expires_at = $5 WHERE id = $1 AND execution_epoch = $2 AND control_revision = $3 AND owner_token = $4`, []any{lease.ID, lease.ExecutionEpoch, lease.ControlRevision, lease.OwnerToken, expiresAt}},
			{`UPDATE agent_turn_slots SET lease_expires_at = $5 WHERE agent_turn_id = $1 AND execution_epoch = $2 AND control_revision = $3 AND owner_token = $4`, []any{lease.ID, lease.ExecutionEpoch, lease.ControlRevision, lease.OwnerToken, expiresAt}},
		}
		for _, update := range updates {
			result, err := tx.Exec(ctx, update.query, update.args...)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return ErrAgentTurnFenceLost
			}
		}
		return nil
	})
}

// RecoverExpiredAgentTurn terminally fences an expired job/turn ownership without reusing its epoch.
func (store *Store) RecoverExpiredAgentTurn(ctx context.Context, turnID string, executionEpoch int64) (AgentTurnRecovery, error) {
	if !validUUID(turnID) || executionEpoch <= 0 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin agent turn recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var jobID string
	if err := tx.QueryRow(ctx, `
SELECT id::text FROM jobs
WHERE kind = 'RUN_AGENT_TURN' AND agent_turn_id = $1 AND execution_epoch = $2`, turnID, executionEpoch).Scan(&jobID); errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	} else if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("look up expired agent turn job: %w", err)
	}
	job, err := lockExpiredAgentTurnJob(ctx, tx, jobID, turnID, executionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if _, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false); err != nil {
		return AgentTurnRecovery{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, turnID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if !turn.active || turn.ExecutionEpoch != executionEpoch ||
		turn.Status != AgentTurnQueued && turn.Status != AgentTurnStarting && turn.Status != AgentTurnRunning {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if turn.Status == AgentTurnRunning && turn.leaseLive {
		return AgentTurnRecovery{}, ErrAgentTurnNotExpired
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnRecovery{}, err
	}
	var slotLive bool
	err = tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM agent_turn_slots WHERE agent_turn_id = $1 FOR UPDATE`, turnID).Scan(&slotLive)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, fmt.Errorf("lock expired agent turn slot: %w", err)
	}
	if err == nil && slotLive {
		return AgentTurnRecovery{}, ErrAgentTurnNotExpired
	}
	if _, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = 'FAILED', finished_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'execution expired before mutation started'
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state = 'RESERVED' AND started_at IS NULL`, turnID, executionEpoch); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("fail unstarted recovered mutations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = 'UNKNOWN', updated_at = clock_timestamp(), last_error = 'execution expired with mutation in flight'
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state = 'IN_FLIGHT'`, turnID, executionEpoch); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("mark in-flight recovered mutations unknown: %w", err)
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, turnID, executionEpoch).Scan(&unsettled); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("check recovery mutations: %w", err)
	}
	status := AgentTurnInterrupted
	if unsettled {
		status = AgentTurnReconciling
	}
	stopJobID, err := enqueueAgentTurnRecoveryJob(ctx, tx, job, turn, StopStaleRuntimeJobKind, stopStaleRuntimeJobPriority)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	reconcileJobID := ""
	if unsettled {
		reconcileJobID, err = enqueueAgentTurnRecoveryJob(ctx, tx, job, turn, ReconcileAgentTurnMutationsJobKind, reconcileTurnMutationsJobPriority)
		if err != nil {
			return AgentTurnRecovery{}, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_turn_slots WHERE agent_turn_id = $1`, turnID); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("release expired agent turn slot: %w", err)
	}
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'EXPIRED', finished_at = clock_timestamp(), retryable = FALSE, last_error = 'agent turn execution lease expired'
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`,
		job.ID, job.AttemptCount, job.LeaseToken)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("expire agent turn job attempt: %w", err)
	}
	if attemptResult.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'FAILED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), last_error = 'agent turn execution lease expired'
WHERE id = $1 AND status = 'LEASED'`, job.ID)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("fail expired agent turn job: %w", err)
	}
	if jobResult.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var recoveryStartedAt time.Time
	turnResult := tx.QueryRow(ctx, `
UPDATE agent_turns
SET status = $2, active = FALSE, mutation_admission_open = FALSE,
    mutation_admission_closed_at = COALESCE(mutation_admission_closed_at, clock_timestamp()),
    owner_id = NULL, owner_token = NULL, leased_at = NULL, lease_expires_at = NULL,
	    heartbeat_at = NULL, completed_at = clock_timestamp(), last_error = 'execution lease expired',
	    recovery_started_at = clock_timestamp(), runtime_stop_required = TRUE,
	    runtime_stopped_at = NULL, recovery_settled_at = NULL,
	    stop_runtime_job_id = $3, reconcile_mutations_job_id = $4
WHERE id = $1 AND active
RETURNING recovery_started_at`, turnID, status, stopJobID, nullableString(reconcileJobID)).Scan(&recoveryStartedAt)
	if err := turnResult; err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("fence expired agent turn: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit agent turn recovery: %w", err)
	}
	return AgentTurnRecovery{
		TurnID: turnID, JobID: job.ID, ExecutionEpoch: executionEpoch, Status: status,
		MutationsUnsettled: unsettled, SuccessorAllowed: false, RuntimeStopRequired: true,
		RecoveryStartedAt: &recoveryStartedAt, StopRuntimeJobID: stopJobID,
		ReconcileMutationsJobID: reconcileJobID,
	}, nil
}

// GetAgentTurnRecovery returns the durable recovery barrier for one fenced epoch.
func (store *Store) GetAgentTurnRecovery(ctx context.Context, turnID string, executionEpoch int64) (AgentTurnRecovery, error) {
	if !validUUID(turnID) || executionEpoch <= 0 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	recovery, err := readAgentTurnRecovery(ctx, store.pool, turnID, executionEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("get Agent Turn recovery: %w", err)
	}
	return recovery, nil
}

// AcknowledgeRecoveredRuntimeStopped records stale Runtime Process termination under its recovery-job fence.
func (store *Store) AcknowledgeRecoveredRuntimeStopped(ctx context.Context, lease JobLease) (AgentTurnRecovery, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin stale runtime acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, StopStaleRuntimeJobKind)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	result, err := tx.Exec(ctx, `
UPDATE agent_turns
SET runtime_stopped_at = COALESCE(runtime_stopped_at, clock_timestamp())
WHERE id = $1 AND agent_session_id = $2 AND execution_epoch = $3
  AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL`,
		job.AgentTurnID, job.AgentSessionID, job.ExecutionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("acknowledge stale runtime stopped: %w", err)
	}
	if result.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnRecoveryFenceLost
	}
	if err := completeRecoveryJobTx(ctx, tx, job, json.RawMessage(`{"runtime_stopped":true}`)); err != nil {
		return AgentTurnRecovery{}, err
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, job.AgentTurnID, job.ExecutionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("read acknowledged recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit stale runtime acknowledgement: %w", err)
	}
	return recovery, nil
}

// ReconcileRecoveredMutation records a known terminal result without the dead execution owner lease.
func (store *Store) ReconcileRecoveredMutation(ctx context.Context, lease JobLease, mutationID string, outcome RecoveredMutationOutcome) (MutationReservation, error) {
	if !validUUID(mutationID) {
		return MutationReservation{}, ErrMutationStateConflict
	}
	var result json.RawMessage
	switch outcome.State {
	case MutationSucceeded:
		var err error
		result, err = canonicalJSON(outcome.Result)
		if err != nil {
			return MutationReservation{}, fmt.Errorf("reconcile recovered mutation: result: %w", err)
		}
		if outcome.LastError != "" {
			return MutationReservation{}, errors.New("reconcile recovered mutation: successful outcome has an error")
		}
	case MutationFailed:
		if strings.TrimSpace(outcome.LastError) == "" || len(outcome.Result) != 0 {
			return MutationReservation{}, errors.New("reconcile recovered mutation: failed outcome requires only an error")
		}
	default:
		return MutationReservation{}, errors.New("reconcile recovered mutation: outcome must be SUCCEEDED or FAILED")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MutationReservation{}, fmt.Errorf("begin recovered mutation reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, _, err := lockAgentTurnRecoveryJob(ctx, tx, lease, ReconcileAgentTurnMutationsJobKind)
	if err != nil {
		return MutationReservation{}, err
	}
	update, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = $4, result = $5, last_error = NULLIF($6, ''),
    updated_at = clock_timestamp(), finished_at = clock_timestamp(),
    duration_ms = CASE WHEN started_at IS NULL THEN NULL
        ELSE GREATEST(0, (EXTRACT(EPOCH FROM (clock_timestamp() - started_at)) * 1000)::bigint) END
WHERE id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')`, mutationID, job.AgentTurnID, job.ExecutionEpoch,
		outcome.State, nullableJSON(result), outcome.LastError)
	if err != nil {
		return MutationReservation{}, fmt.Errorf("reconcile recovered mutation: %w", err)
	}
	if update.RowsAffected() != 1 {
		return MutationReservation{}, ErrMutationStateConflict
	}
	mutation, err := getMutationByID(ctx, tx, mutationID)
	if err != nil {
		return MutationReservation{}, fmt.Errorf("read reconciled mutation: %w", err)
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, job.AgentTurnID, job.ExecutionEpoch).Scan(&unsettled); err != nil {
		return MutationReservation{}, fmt.Errorf("check remaining recovered mutations: %w", err)
	}
	if !unsettled {
		if err := completeRecoveryJobTx(ctx, tx, job, json.RawMessage(`{"mutations_reconciled":true}`)); err != nil {
			return MutationReservation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationReservation{}, fmt.Errorf("commit recovered mutation reconciliation: %w", err)
	}
	return mutation, nil
}

// CompleteAgentTurnRecovery opens successor allocation after every recovery barrier is durable.
func (store *Store) CompleteAgentTurnRecovery(ctx context.Context, turnID string, executionEpoch int64) (AgentTurnRecovery, error) {
	if !validUUID(turnID) || executionEpoch <= 0 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin Agent Turn recovery completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var hierarchy hierarchyIdentity
	var stopJobID, reconcileJobID string
	err = tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, turn.workflow_attempt_id::text,
       assignment.id::text, session.id::text, turn.stop_runtime_job_id::text,
       COALESCE(turn.reconcile_mutations_job_id::text, '')
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND turn.recovery_started_at IS NOT NULL`,
		turnID, executionEpoch).Scan(&hierarchy.workflowID, &hierarchy.attemptID,
		&hierarchy.assignmentID, &hierarchy.sessionID, &stopJobID, &reconcileJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("look up Agent Turn recovery completion: %w", err)
	}
	var stopStatus JobStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, stopJobID).Scan(&stopStatus); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock stale runtime stop job: %w", err)
	}
	reconcileStatus := JobSucceeded
	if reconcileJobID != "" {
		if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, reconcileJobID).Scan(&reconcileStatus); err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("lock mutation reconciliation job: %w", err)
		}
	}
	if _, err := lockHierarchy(ctx, tx, hierarchy, hierarchy.attemptID, false); err != nil {
		return AgentTurnRecovery{}, err
	}
	if _, err := lockAgentTurn(ctx, tx, turnID); err != nil {
		return AgentTurnRecovery{}, err
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnRecovery{}, err
	}
	var stopped, alreadySettled bool
	if err := tx.QueryRow(ctx, `
SELECT runtime_stopped_at IS NOT NULL, recovery_settled_at IS NOT NULL
FROM agent_turns WHERE id = $1 AND execution_epoch = $2`, turnID, executionEpoch).Scan(&stopped, &alreadySettled); err != nil {
		return AgentTurnRecovery{}, err
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, turnID, executionEpoch).Scan(&unsettled); err != nil {
		return AgentTurnRecovery{}, err
	}
	if !stopped || stopStatus != JobSucceeded || reconcileStatus != JobSucceeded || unsettled {
		return AgentTurnRecovery{}, ErrAgentTurnRecoveryUnsettled
	}
	if !alreadySettled {
		result, err := tx.Exec(ctx, `
UPDATE agent_turns SET recovery_settled_at = clock_timestamp()
WHERE id = $1 AND execution_epoch = $2 AND recovery_started_at IS NOT NULL
  AND recovery_settled_at IS NULL`, turnID, executionEpoch)
		if err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("settle Agent Turn recovery: %w", err)
		}
		if result.RowsAffected() != 1 {
			return AgentTurnRecovery{}, ErrAgentTurnRecoveryUnsettled
		}
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, turnID, executionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("read settled Agent Turn recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit Agent Turn recovery completion: %w", err)
	}
	return recovery, nil
}

// OpenMutationAdmission allows the live, job-bound owner to reserve side effects.
func (store *Store) OpenMutationAdmission(ctx context.Context, lease AgentTurnLease) error {
	return store.withLockedAgentTurnLease(ctx, lease, "open mutation admission", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		_, err := tx.Exec(ctx, `UPDATE agent_turns SET mutation_admission_open = TRUE, mutation_admission_closed_at = NULL WHERE id = $1`, lease.ID)
		return err
	})
}

// CloseMutationAdmission serializes with reservation and starts the settlement barrier.
func (store *Store) CloseMutationAdmission(ctx context.Context, lease AgentTurnLease) error {
	return store.withLockedAgentTurnLease(ctx, lease, "close mutation admission", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning && turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling {
			return ErrAgentTurnFenceLost
		}
		var reconciliation bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM tool_invocations WHERE agent_turn_id = $1 AND execution_epoch = $2
AND kind = 'MUTATION' AND state IN ('UNKNOWN', 'RECONCILING'))`, lease.ID, lease.ExecutionEpoch).Scan(&reconciliation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
UPDATE agent_turns SET mutation_admission_open = FALSE,
mutation_admission_closed_at = COALESCE(mutation_admission_closed_at, clock_timestamp()),
status = CASE WHEN $2 THEN 'RECONCILING' ELSE 'SETTLING' END WHERE id = $1`, lease.ID, reconciliation)
		return err
	})
}

// ValidateTurnFence checks the live job attempt, hierarchy, turn owner, and global slot.
func (store *Store) ValidateTurnFence(ctx context.Context, lease AgentTurnLease) error {
	return store.withLockedAgentTurnLeaseStatus(ctx, lease, "validate agent turn fence", true, func(pgx.Tx, lockedTurn) error { return nil })
}

// ValidateAgentSessionPromptFence checks a live Agent Turn lease and its exact durable ACP Session identity.
func (store *Store) ValidateAgentSessionPromptFence(ctx context.Context, lease AgentTurnLease, acpSessionID string) error {
	return store.withLockedAgentTurnLease(ctx, lease, "validate Agent Session prompt fence", func(tx pgx.Tx, _ lockedTurn) error {
		var persisted string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(acp_session_id, '') FROM agent_sessions WHERE id = $1`, lease.AgentSessionID).Scan(&persisted); err != nil {
			return err
		}
		if persisted != acpSessionID {
			return ErrAgentSessionACPConflict
		}
		return nil
	})
}

// ReserveMutation allocates a stable operation and monotonic invocation before a side effect.
func (store *Store) ReserveMutation(ctx context.Context, lease AgentTurnLease, spec MutationSpec) (MutationReservation, error) {
	request, err := validateMutationSpec(spec)
	if err != nil {
		return MutationReservation{}, err
	}
	spec.Request = request
	invocationID, err := randomUUID()
	if err != nil {
		return MutationReservation{}, fmt.Errorf("reserve mutation: %w", err)
	}
	var reservation MutationReservation
	err = store.withLockedAgentTurnLease(ctx, lease, "reserve mutation", func(tx pgx.Tx, turn lockedTurn) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, spec.OperationID); err != nil {
			return err
		}
		existing, err := getMutationByOperation(ctx, tx, spec.OperationID)
		if err == nil {
			if !sameMutationDefinition(existing, lease, spec) {
				return ErrMutationOperationConflict
			}
			reservation = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
			return ErrMutationAdmissionClosed
		}
		var number int64
		if err := tx.QueryRow(ctx, `UPDATE agent_turns SET next_invocation_number = next_invocation_number + 1 WHERE id = $1 RETURNING next_invocation_number - 1`, lease.ID).Scan(&number); err != nil {
			return err
		}
		reservation = MutationReservation{
			ID: invocationID, AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch,
			InvocationNumber: number, OperationID: spec.OperationID, ToolName: spec.ToolName,
			Request: spec.Request, State: MutationReserved, ExternalService: spec.ExternalService,
			ExternalResourceID: spec.ExternalResourceID, ExpectedSHA: spec.ExpectedSHA,
		}
		return tx.QueryRow(ctx, `
INSERT INTO tool_invocations (
    id, agent_turn_id, execution_epoch, invocation_number, tool_name, kind, state,
    idempotency_key, operation_id, request, external_service, external_resource_id, expected_sha
)
VALUES ($1, $2, $3, $4, $5, 'MUTATION', 'RESERVED', $6, $6, $7, $8, $9, $10)
RETURNING admitted_at`, invocationID, lease.ID, lease.ExecutionEpoch, number, spec.ToolName,
			spec.OperationID, spec.Request, nullableString(spec.ExternalService),
			nullableString(spec.ExternalResourceID), nullableString(spec.ExpectedSHA)).Scan(&reservation.AdmittedAt)
	})
	return reservation, err
}

// StartMutation moves one reservation to IN_FLIGHT using a fenced compare-and-swap.
func (store *Store) StartMutation(ctx context.Context, lease AgentTurnLease, mutationID string) (MutationReservation, error) {
	if !validUUID(mutationID) {
		return MutationReservation{}, ErrMutationStateConflict
	}
	var mutation MutationReservation
	err := store.withLockedAgentTurnLease(ctx, lease, "start mutation", func(tx pgx.Tx, _ lockedTurn) error {
		result, err := tx.Exec(ctx, `
UPDATE tool_invocations SET state = 'IN_FLIGHT', started_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 AND kind = 'MUTATION' AND state = 'RESERVED'`,
			mutationID, lease.ID, lease.ExecutionEpoch)
		if err != nil || result.RowsAffected() != 1 {
			if err == nil {
				err = ErrMutationStateConflict
			}
			return err
		}
		mutation, err = getMutationByID(ctx, tx, mutationID)
		return err
	})
	return mutation, err
}

// CompleteMutation records a known successful outcome from IN_FLIGHT or RECONCILING.
func (store *Store) CompleteMutation(ctx context.Context, lease AgentTurnLease, mutationID string, result json.RawMessage) error {
	canonical, err := canonicalJSON(result)
	if err != nil {
		return fmt.Errorf("complete mutation: result: %w", err)
	}
	return store.mutateMutationState(ctx, lease, mutationID, "complete mutation", []MutationState{MutationInFlight, MutationReconciling}, MutationSucceeded, canonical, nil)
}

// FailMutation records a known terminal failure from an admitted active state.
func (store *Store) FailMutation(ctx context.Context, lease AgentTurnLease, mutationID string, cause error) error {
	if cause == nil {
		return errors.New("fail mutation: cause is nil")
	}
	return store.mutateMutationState(ctx, lease, mutationID, "fail mutation", []MutationState{MutationReserved, MutationInFlight, MutationReconciling}, MutationFailed, nil, cause)
}

// MarkMutationUnknown records an ambiguous external result that requires reconciliation.
func (store *Store) MarkMutationUnknown(ctx context.Context, lease AgentTurnLease, mutationID string, cause error) error {
	if cause == nil {
		return errors.New("mark mutation unknown: cause is nil")
	}
	return store.mutateMutationState(ctx, lease, mutationID, "mark mutation unknown", []MutationState{MutationInFlight}, MutationUnknown, nil, cause)
}

// BeginMutationReconciliation moves an UNKNOWN mutation into active reconciliation.
func (store *Store) BeginMutationReconciliation(ctx context.Context, lease AgentTurnLease, mutationID string) error {
	return store.mutateMutationState(ctx, lease, mutationID, "begin mutation reconciliation", []MutationState{MutationUnknown}, MutationReconciling, nil, nil)
}

// ListUnsettledMutations returns every non-terminal mutation under a validated fence.
func (store *Store) ListUnsettledMutations(ctx context.Context, lease AgentTurnLease) ([]MutationReservation, error) {
	var mutations []MutationReservation
	err := store.withLockedAgentTurnLease(ctx, lease, "list unsettled mutations", func(tx pgx.Tx, _ lockedTurn) error {
		rows, err := tx.Query(ctx, mutationSelect+`
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
ORDER BY invocation_number`, lease.ID, lease.ExecutionEpoch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			mutation, err := scanMutation(rows)
			if err != nil {
				return err
			}
			mutations = append(mutations, mutation)
		}
		return rows.Err()
	})
	return mutations, err
}

// FinalizeAgentTurn settles both the turn and its bound execution job after all mutations terminate.
func (store *Store) FinalizeAgentTurn(ctx context.Context, lease AgentTurnLease, completion AgentTurnCompletion) error {
	if !terminalAgentTurnStatus(completion.Status) {
		return errors.New("finalize agent turn: status is not terminal")
	}
	var outcome json.RawMessage
	var err error
	if len(completion.Outcome) != 0 {
		outcome, err = canonicalJSON(completion.Outcome)
		if err != nil {
			return fmt.Errorf("finalize agent turn: outcome: %w", err)
		}
	}
	return store.withLockedAgentTurnLease(ctx, lease, "finalize agent turn", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.MutationAdmissionOpen || turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling {
			return ErrMutationAdmissionClosed
		}
		var unsettled bool
		if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, lease.ID, lease.ExecutionEpoch).Scan(&unsettled); err != nil {
			return err
		}
		if unsettled {
			return ErrAgentTurnMutationsUnsettled
		}
		jobResult, err := json.Marshal(map[string]any{"turn_status": completion.Status, "outcome": json.RawMessage(outcome)})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agent_turn_slots WHERE agent_turn_id = $1 AND owner_token = $2`, lease.ID, lease.OwnerToken); err != nil {
			return err
		}
		attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
			lease.JobLease.ID, lease.JobLease.LeaseToken, lease.JobLease.Attempt, jobResult)
		if err != nil {
			return err
		}
		if attemptResult.RowsAffected() != 1 {
			return ErrAgentTurnFenceLost
		}
		jobUpdate, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`,
			lease.JobLease.ID, lease.JobLease.LeaseToken, lease.JobLease.Attempt, jobResult)
		if err != nil {
			return err
		}
		if jobUpdate.RowsAffected() != 1 {
			return ErrAgentTurnFenceLost
		}
		turnUpdate, err := tx.Exec(ctx, `
UPDATE agent_turns SET status = $5, active = FALSE, owner_id = NULL, owner_token = NULL,
leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL, completed_at = clock_timestamp(),
outcome = $6, last_error = NULLIF($7, '')
WHERE id = $1 AND execution_epoch = $2 AND control_revision = $3 AND owner_token = $4`,
			lease.ID, lease.ExecutionEpoch, lease.ControlRevision, lease.OwnerToken,
			completion.Status, nullableJSON(outcome), completion.LastError)
		if err == nil && turnUpdate.RowsAffected() != 1 {
			return ErrAgentTurnFenceLost
		}
		return err
	})
}

// RuntimeLabels returns non-secret Runtime Process labels carrying mandatory turn identity.
func RuntimeLabels(turn AgentTurn) map[string]string {
	return map[string]string{
		RuntimeLabelAssignmentID: turn.AgentAssignmentID,
		RuntimeLabelSessionID:    turn.AgentSessionID,
		RuntimeLabelTurnID:       turn.ID,
		RuntimeLabelEpoch:        strconv.FormatInt(turn.ExecutionEpoch, 10),
	}
}

type hierarchyIdentity struct {
	workflowID, attemptID, assignmentID, sessionID string
}

type lockedHierarchy struct {
	controlRevision                    int64
	nextTurnNumber, nextExecutionEpoch int64
	workflowStatus                     string
	attemptActive                      bool
	assignmentStatus                   string
	sessionStatus                      AgentSessionStatus
	controlOwner                       SessionControlOwner
	acpSessionID                       string
}

type lockedTurn struct {
	AgentTurn
	active, leasePresent, leaseLive bool
	ownerID, ownerToken             string
}

func lookupSessionHierarchy(ctx context.Context, tx pgx.Tx, sessionID string) (hierarchyIdentity, error) {
	var hierarchy hierarchyIdentity
	err := tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, assignment.id::text, session.id::text
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE session.id = $1`, sessionID).Scan(&hierarchy.workflowID, &hierarchy.assignmentID, &hierarchy.sessionID)
	return hierarchy, err
}

func lockHierarchy(ctx context.Context, tx pgx.Tx, hierarchy hierarchyIdentity, attemptID string, requireActive bool) (lockedHierarchy, error) {
	var locked lockedHierarchy
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`, hierarchy.workflowID).Scan(&locked.workflowStatus); err != nil {
		return lockedHierarchy{}, hierarchyLockError("workflow", err)
	}
	if err := tx.QueryRow(ctx, `SELECT active FROM workflow_attempts WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, attemptID, hierarchy.workflowID).Scan(&locked.attemptActive); err != nil {
		return lockedHierarchy{}, hierarchyLockError("workflow attempt", err)
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM agent_assignments WHERE id = $1 AND workflow_id = $2 FOR UPDATE`, hierarchy.assignmentID, hierarchy.workflowID).Scan(&locked.assignmentStatus); err != nil {
		return lockedHierarchy{}, hierarchyLockError("agent assignment", err)
	}
	if err := tx.QueryRow(ctx, `
SELECT status, control_owner, control_revision, next_turn_number, next_execution_epoch,
       COALESCE(acp_session_id, '')
FROM agent_sessions WHERE id = $1 AND agent_assignment_id = $2 FOR UPDATE`,
		hierarchy.sessionID, hierarchy.assignmentID).Scan(&locked.sessionStatus, &locked.controlOwner,
		&locked.controlRevision, &locked.nextTurnNumber, &locked.nextExecutionEpoch, &locked.acpSessionID); err != nil {
		return lockedHierarchy{}, hierarchyLockError("agent session", err)
	}
	if requireActive && !locked.allowsActiveTurn() {
		return lockedHierarchy{}, ErrAgentTurnHierarchyInactive
	}
	return locked, nil
}

func (locked lockedHierarchy) allowsActiveTurn() bool {
	return workflowAllowsTurns(locked.workflowStatus) && locked.attemptActive &&
		locked.assignmentStatus == "ACTIVE" && locked.sessionStatus == AgentSessionActive &&
		locked.controlOwner == SessionControlAutomation
}

func (locked lockedHierarchy) allowsAcquisition(turn AgentTurn) bool {
	if locked.allowsActiveTurn() {
		return true
	}
	return workflowAllowsTurns(locked.workflowStatus) && locked.attemptActive &&
		locked.assignmentStatus == "ACTIVE" && locked.sessionStatus == AgentSessionCreating &&
		locked.controlOwner == SessionControlAutomation && locked.acpSessionID == ""
}

func lockAgentTurnJob(ctx context.Context, tx pgx.Tx, lease JobLease, requireLive bool) (Job, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return Job{}, ErrAgentTurnFenceLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("lock agent turn job: %w", err)
	}
	if job.Kind != RunAgentTurnJobKind || job.MaxAttempts != 1 || job.AgentTurnID == "" || job.ExecutionEpoch <= 0 ||
		job.Status != JobLeased || job.LeaseToken != lease.LeaseToken || job.AttemptCount != lease.Attempt {
		return Job{}, ErrAgentTurnFenceLost
	}
	var attemptLive bool
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.Attempt, lease.LeaseToken).Scan(&attemptLive)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && requireLive && (!attemptLive || job.LeaseExpiresAt == nil) {
		return Job{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("lock agent turn job attempt: %w", err)
	}
	if requireLive {
		var jobLive bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil || !jobLive {
			return Job{}, ErrAgentTurnFenceLost
		}
	}
	return job, nil
}

func lockExpiredAgentTurnJob(ctx context.Context, tx pgx.Tx, jobID, turnID string, epoch int64) (Job, error) {
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID))
	if err != nil {
		return Job{}, fmt.Errorf("lock expired agent turn job: %w", err)
	}
	if job.Kind != RunAgentTurnJobKind || job.Status != JobLeased || job.AgentTurnID != turnID || job.ExecutionEpoch != epoch || job.LeaseExpiresAt == nil {
		return Job{}, ErrAgentTurnFenceLost
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at <= clock_timestamp() FROM jobs WHERE id = $1`, jobID).Scan(&expired); err != nil {
		return Job{}, err
	}
	if !expired {
		return Job{}, ErrAgentTurnNotExpired
	}
	var attemptExpired bool
	if err := tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_expires_at <= clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 FOR UPDATE`,
		job.ID, job.AttemptCount, job.LeaseToken).Scan(&attemptExpired); err != nil || !attemptExpired {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			return Job{}, ErrAgentTurnNotExpired
		}
		return Job{}, err
	}
	return job, nil
}

func hierarchyFromJob(job Job) hierarchyIdentity {
	return hierarchyIdentity{
		workflowID: job.WorkflowID, attemptID: job.WorkflowAttemptID,
		assignmentID: job.AgentAssignmentID, sessionID: job.AgentSessionID,
	}
}

type recoveryJobIdentity struct {
	WorkflowID        string `json:"workflow_id"`
	WorkflowAttemptID string `json:"workflow_attempt_id"`
	AgentAssignmentID string `json:"agent_assignment_id"`
	AgentSessionID    string `json:"agent_session_id"`
	AgentTurnID       string `json:"agent_turn_id"`
	ExecutionEpoch    int64  `json:"execution_epoch"`
	ControlRevision   int64  `json:"control_revision"`
	ExecutionJobID    string `json:"execution_job_id"`
}

func enqueueAgentTurnRecoveryJob(ctx context.Context, tx pgx.Tx, executionJob Job, turn lockedTurn, kind string, priority int) (string, error) {
	id, err := randomUUID()
	if err != nil {
		return "", fmt.Errorf("generate Agent Turn recovery job ID: %w", err)
	}
	identity := recoveryJobIdentity{
		WorkflowID: executionJob.WorkflowID, WorkflowAttemptID: executionJob.WorkflowAttemptID,
		AgentAssignmentID: executionJob.AgentAssignmentID, AgentSessionID: executionJob.AgentSessionID,
		AgentTurnID: executionJob.AgentTurnID, ExecutionEpoch: executionJob.ExecutionEpoch,
		ControlRevision: turn.ControlRevision, ExecutionJobID: executionJob.ID,
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode Agent Turn recovery job: %w", err)
	}
	idempotencyKey := fmt.Sprintf("agent-turn-recovery:%s:epoch:%d:%s", executionJob.AgentTurnID, executionJob.ExecutionEpoch, strings.ToLower(kind))
	result, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', $5, clock_timestamp(), $6,
        $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		id, AgentTurnRecoveryQueue, kind, payload, priority, agentTurnRecoveryJobMaxAttempts,
		idempotencyKey, executionJob.WorkflowID, executionJob.WorkflowAttemptID,
		executionJob.AgentAssignmentID, executionJob.AgentSessionID, executionJob.AgentTurnID,
		executionJob.ExecutionEpoch)
	if err != nil {
		return "", fmt.Errorf("enqueue %s job: %w", kind, err)
	}
	if result.RowsAffected() == 1 {
		return id, nil
	}
	var existingID, existingKind string
	var existingPayload []byte
	if err := tx.QueryRow(ctx, `SELECT id::text, kind, payload FROM jobs WHERE idempotency_key = $1`, idempotencyKey).Scan(&existingID, &existingKind, &existingPayload); err != nil {
		return "", fmt.Errorf("read existing %s job: %w", kind, err)
	}
	canonicalExisting, _ := canonicalJSON(existingPayload)
	canonicalPayload, _ := canonicalJSON(payload)
	if existingKind != kind || !bytes.Equal(canonicalExisting, canonicalPayload) {
		return "", ErrJobIdempotencyConflict
	}
	return existingID, nil
}

func lockAgentTurnRecoveryJob(ctx context.Context, tx pgx.Tx, lease JobLease, expectedKind string) (Job, lockedTurn, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	if job.Kind != expectedKind || job.Queue != AgentTurnRecoveryQueue || job.Status != JobLeased ||
		job.LeaseToken != lease.LeaseToken || job.AttemptCount != lease.Attempt ||
		job.WorkflowID != lease.WorkflowID || job.WorkflowAttemptID != lease.WorkflowAttemptID ||
		job.AgentAssignmentID != lease.AgentAssignmentID || job.AgentSessionID != lease.AgentSessionID ||
		job.AgentTurnID != lease.AgentTurnID || job.ExecutionEpoch != lease.ExecutionEpoch {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	var jobLive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil || !jobLive {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	var attemptLive bool
	if err := tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.Attempt, lease.LeaseToken).Scan(&attemptLive); err != nil || !attemptLive {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	if _, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false); err != nil {
		return Job{}, lockedTurn{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, job.AgentTurnID)
	if err != nil {
		return Job{}, lockedTurn{}, err
	}
	var identity recoveryJobIdentity
	if err := json.Unmarshal(job.Payload, &identity); err != nil ||
		identity.WorkflowID != job.WorkflowID || identity.WorkflowAttemptID != job.WorkflowAttemptID ||
		identity.AgentAssignmentID != job.AgentAssignmentID || identity.AgentSessionID != job.AgentSessionID ||
		identity.AgentTurnID != job.AgentTurnID || identity.ExecutionEpoch != job.ExecutionEpoch ||
		identity.ControlRevision != turn.ControlRevision || identity.ExecutionJobID == "" ||
		turn.active || turn.Status != AgentTurnInterrupted && turn.Status != AgentTurnReconciling {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	var recoveryLive bool
	var registeredJobID string
	var runtimeStopped bool
	if err := tx.QueryRow(ctx, `
SELECT recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL,
       CASE WHEN $3 = 'STOP_STALE_RUNTIME' THEN stop_runtime_job_id::text
            ELSE reconcile_mutations_job_id::text END,
       runtime_stopped_at IS NOT NULL
FROM agent_turns WHERE id = $1 AND execution_epoch = $2`,
		job.AgentTurnID, job.ExecutionEpoch, expectedKind).Scan(&recoveryLive, &registeredJobID, &runtimeStopped); err != nil || !recoveryLive || registeredJobID != job.ID {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	if expectedKind == ReconcileAgentTurnMutationsJobKind && !runtimeStopped {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryUnsettled
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return Job{}, lockedTurn{}, err
	}
	return job, turn, nil
}

func completeRecoveryJobTx(ctx context.Context, tx pgx.Tx, job Job, result json.RawMessage) error {
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, result)
	if err != nil {
		return fmt.Errorf("complete recovery job attempt: %w", err)
	}
	if attemptResult.RowsAffected() != 1 {
		return ErrAgentTurnRecoveryFenceLost
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, result)
	if err != nil {
		return fmt.Errorf("complete recovery job: %w", err)
	}
	if jobResult.RowsAffected() != 1 {
		return ErrAgentTurnRecoveryFenceLost
	}
	return nil
}

type recoveryQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readAgentTurnRecovery(ctx context.Context, queryer recoveryQueryer, turnID string, epoch int64) (AgentTurnRecovery, error) {
	var recovery AgentTurnRecovery
	err := queryer.QueryRow(ctx, `
SELECT turn.id::text, execution_job.id::text, turn.execution_epoch, turn.status,
       turn.runtime_stop_required, turn.recovery_started_at, turn.runtime_stopped_at,
       turn.recovery_settled_at, turn.stop_runtime_job_id::text,
       COALESCE(turn.reconcile_mutations_job_id::text, '')
FROM agent_turns AS turn
JOIN jobs AS execution_job ON execution_job.agent_turn_id = turn.id
    AND execution_job.execution_epoch = turn.execution_epoch
    AND execution_job.kind = 'RUN_AGENT_TURN'
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND turn.recovery_started_at IS NOT NULL`,
		turnID, epoch).Scan(&recovery.TurnID, &recovery.JobID, &recovery.ExecutionEpoch,
		&recovery.Status, &recovery.RuntimeStopRequired, &recovery.RecoveryStartedAt,
		&recovery.RuntimeStoppedAt, &recovery.RecoverySettledAt,
		&recovery.StopRuntimeJobID, &recovery.ReconcileMutationsJobID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if err := queryer.QueryRow(ctx, unsettledMutationsForTurnSQL, turnID, epoch).Scan(&recovery.MutationsUnsettled); err != nil {
		return AgentTurnRecovery{}, err
	}
	recovery.SuccessorAllowed = recovery.RecoverySettledAt != nil
	return recovery, nil
}

func isAgentTurnRecoveryJob(kind string) bool {
	return kind == StopStaleRuntimeJobKind || kind == ReconcileAgentTurnMutationsJobKind
}

func lockAgentTurn(ctx context.Context, tx pgx.Tx, turnID string) (lockedTurn, error) {
	var turn lockedTurn
	var profileConfig []byte
	err := tx.QueryRow(ctx, `
	SELECT id::text, agent_session_id::text, workflow_attempt_id::text, turn_number,
	execution_epoch, control_revision, status, mutation_admission_open, active,
	COALESCE(owner_id, ''), COALESCE(owner_token::text, ''), lease_expires_at IS NOT NULL,
	COALESCE(lease_expires_at > clock_timestamp(), FALSE), COALESCE(retry_of_turn_id::text, ''),
	COALESCE(purpose, ''), COALESCE(change_proposal_id::text, ''), COALESCE(expected_head_sha, ''),
	agent_profile_commit_sha, agent_profile_content_sha256, agent_profile_config, created_at
FROM agent_turns WHERE id = $1 FOR UPDATE`, turnID).Scan(
		&turn.ID, &turn.AgentSessionID, &turn.WorkflowAttemptID, &turn.TurnNumber,
		&turn.ExecutionEpoch, &turn.ControlRevision, &turn.Status, &turn.MutationAdmissionOpen,
		&turn.active, &turn.ownerID, &turn.ownerToken, &turn.leasePresent, &turn.leaseLive,
		&turn.RetryOfTurnID, &turn.Purpose, &turn.ChangeProposalID, &turn.ExpectedHeadSHA,
		&turn.AgentProfileCommitSHA, &turn.AgentProfileContentSHA256,
		&profileConfig, &turn.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedTurn{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return lockedTurn{}, fmt.Errorf("lock agent turn: %w", err)
	}
	turn.AgentProfileConfig, err = canonicalJSON(profileConfig)
	return turn, err
}

func lockAgentTurnSlots(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, agentTurnSlotsLockID); err != nil {
		return fmt.Errorf("lock agent turn slots: %w", err)
	}
	return nil
}

func (store *Store) withLockedAgentTurnLease(ctx context.Context, lease AgentTurnLease, action string, operation func(pgx.Tx, lockedTurn) error) error {
	return store.withLockedAgentTurnLeaseStatus(ctx, lease, action, false, operation)
}

func (store *Store) withLockedAgentTurnLeaseStatus(ctx context.Context, lease AgentTurnLease, action string, allowCreating bool, operation func(pgx.Tx, lockedTurn) error) error {
	if !validUUID(lease.ID) || !validUUID(lease.AgentSessionID) || !validUUID(lease.OwnerToken) ||
		lease.ExecutionEpoch <= 0 || lease.ControlRevision <= 0 || strings.TrimSpace(lease.OwnerID) == "" {
		return ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockAgentTurnJob(ctx, tx, lease.JobLease, true)
	if err != nil {
		return ErrAgentTurnFenceLost
	}
	if job.AgentTurnID != lease.ID || job.AgentSessionID != lease.AgentSessionID || job.ExecutionEpoch != lease.ExecutionEpoch {
		return ErrAgentTurnFenceLost
	}
	lockedHierarchy, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false)
	if err != nil {
		return err
	}
	turn, err := lockAgentTurn(ctx, tx, lease.ID)
	if err != nil {
		return err
	}
	turn.AgentAssignmentID = job.AgentAssignmentID
	if !lockedHierarchy.allowsActiveTurn() && !(allowCreating && lockedHierarchy.allowsAcquisition(turn.AgentTurn)) {
		return ErrAgentTurnFenceLost
	}
	if !turn.active || lockedHierarchy.controlRevision != lease.ControlRevision ||
		turn.AgentSessionID != lease.AgentSessionID || turn.ExecutionEpoch != lease.ExecutionEpoch ||
		turn.ControlRevision != lease.ControlRevision || turn.ownerID != lease.OwnerID ||
		turn.ownerToken != lease.OwnerToken || !turn.leaseLive {
		return ErrAgentTurnFenceLost
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return err
	}
	var slotLive bool
	err = tx.QueryRow(ctx, `
SELECT lease_expires_at > clock_timestamp() FROM agent_turn_slots
WHERE agent_turn_id = $1 AND agent_session_id = $2 AND execution_epoch = $3
AND control_revision = $4 AND owner_id = $5 AND owner_token = $6 FOR UPDATE`,
		lease.ID, lease.AgentSessionID, lease.ExecutionEpoch, lease.ControlRevision,
		lease.OwnerID, lease.OwnerToken).Scan(&slotLive)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !slotLive {
		return ErrAgentTurnFenceLost
	}
	if err != nil {
		return fmt.Errorf("lock slot for %s: %w", action, err)
	}
	if err := operation(tx, turn); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", action, err)
	}
	return nil
}

func (store *Store) mutateMutationState(ctx context.Context, lease AgentTurnLease, mutationID, action string, from []MutationState, to MutationState, result json.RawMessage, cause error) error {
	if !validUUID(mutationID) {
		return ErrMutationStateConflict
	}
	return store.withLockedAgentTurnLease(ctx, lease, action, func(tx pgx.Tx, _ lockedTurn) error {
		resultValue := nullableJSON(result)
		lastError := ""
		if cause != nil {
			lastError = cause.Error()
		}
		resultTag, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = $4, result = $5, last_error = NULLIF($6, ''), updated_at = clock_timestamp(),
finished_at = CASE WHEN $4 IN ('SUCCEEDED', 'FAILED') THEN clock_timestamp() ELSE NULL END,
duration_ms = CASE WHEN $4 IN ('SUCCEEDED', 'FAILED') AND started_at IS NOT NULL
THEN GREATEST(0, (EXTRACT(EPOCH FROM (clock_timestamp() - started_at)) * 1000)::bigint) ELSE NULL END
WHERE id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 AND kind = 'MUTATION' AND state = ANY($7)`,
			mutationID, lease.ID, lease.ExecutionEpoch, to, resultValue, lastError, mutationStates(from))
		if err != nil {
			return err
		}
		if resultTag.RowsAffected() != 1 {
			return ErrMutationStateConflict
		}
		return nil
	})
}

const mutationSelect = `
SELECT id::text, agent_turn_id::text, execution_epoch, invocation_number,
COALESCE(operation_id, ''), tool_name, request, state,
COALESCE(external_service, ''), COALESCE(external_resource_id, ''), COALESCE(expected_sha, ''),
result, COALESCE(last_error, ''), admitted_at, started_at, finished_at
FROM tool_invocations`

func getMutationByOperation(ctx context.Context, tx pgx.Tx, operationID string) (MutationReservation, error) {
	return scanMutation(tx.QueryRow(ctx, mutationSelect+` WHERE operation_id = $1`, operationID))
}

func getMutationByID(ctx context.Context, tx pgx.Tx, mutationID string) (MutationReservation, error) {
	return scanMutation(tx.QueryRow(ctx, mutationSelect+` WHERE id = $1`, mutationID))
}

func scanMutation(row rowScanner) (MutationReservation, error) {
	var mutation MutationReservation
	var request, result []byte
	err := row.Scan(
		&mutation.ID, &mutation.AgentTurnID, &mutation.ExecutionEpoch, &mutation.InvocationNumber,
		&mutation.OperationID, &mutation.ToolName, &request, &mutation.State,
		&mutation.ExternalService, &mutation.ExternalResourceID, &mutation.ExpectedSHA,
		&result, &mutation.LastError, &mutation.AdmittedAt, &mutation.StartedAt, &mutation.FinishedAt,
	)
	if err != nil {
		return MutationReservation{}, err
	}
	mutation.Request, err = canonicalJSON(request)
	if err != nil {
		return MutationReservation{}, err
	}
	mutation.Result = json.RawMessage(result)
	return mutation, nil
}

func sameMutationDefinition(existing MutationReservation, lease AgentTurnLease, spec MutationSpec) bool {
	return existing.AgentTurnID == lease.ID && existing.ExecutionEpoch == lease.ExecutionEpoch &&
		existing.ToolName == spec.ToolName && bytes.Equal(existing.Request, spec.Request) &&
		existing.ExternalService == spec.ExternalService && existing.ExternalResourceID == spec.ExternalResourceID &&
		existing.ExpectedSHA == spec.ExpectedSHA
}

func validateAgentTurnSpec(spec AgentTurnSpec) (json.RawMessage, error) {
	if !validUUID(spec.AgentSessionID) || !validUUID(spec.WorkflowAttemptID) || spec.RetryOfTurnID != "" && !validUUID(spec.RetryOfTurnID) {
		return nil, errors.New("allocate agent turn: invalid identity")
	}
	if spec.ControlRevision <= 0 {
		return nil, errors.New("allocate agent turn: control revision must be positive")
	}
	switch spec.Purpose {
	case workflow.TurnPurposeInitialDevelopment, workflow.TurnPurposeReview,
		workflow.TurnPurposeRequestedChanges, workflow.TurnPurposeRetry,
		workflow.TurnPurposeSynchronization, workflow.TurnPurposeReactivation:
	default:
		return nil, errors.New("allocate agent turn: invalid purpose")
	}
	if (spec.ChangeProposalID == "") != (spec.ExpectedHeadSHA == "") || spec.ChangeProposalID != "" && !validUUID(spec.ChangeProposalID) {
		return nil, errors.New("allocate agent turn: invalid Change Proposal relation")
	}
	if strings.TrimSpace(spec.AgentProfileCommitSHA) == "" || len(spec.AgentProfileContentSHA256) != 32 {
		return nil, errors.New("allocate agent turn: invalid agent profile identity")
	}
	config, err := canonicalJSON(spec.AgentProfileConfig)
	if err != nil {
		return nil, fmt.Errorf("allocate agent turn: profile config: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(config, &decoded); err != nil {
		return nil, errors.New("allocate agent turn: invalid profile config")
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, errors.New("allocate agent turn: profile config must be an object")
	}
	if containsCredential(decoded) {
		return nil, errors.New("allocate agent turn: profile config contains credentials")
	}
	return config, nil
}

func validateMutationSpec(spec MutationSpec) (json.RawMessage, error) {
	if strings.TrimSpace(spec.OperationID) == "" || strings.TrimSpace(spec.ToolName) == "" {
		return nil, errors.New("reserve mutation: operation ID and tool name are required")
	}
	request, err := canonicalJSON(spec.Request)
	if err != nil {
		return nil, fmt.Errorf("reserve mutation: request: %w", err)
	}
	return request, nil
}

func hierarchyLockError(entity string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentTurnHierarchyInactive
	}
	return fmt.Errorf("lock %s hierarchy: %w", entity, err)
}

func workflowAllowsTurns(status string) bool {
	return status != "CLOSING" && status != "CLOSED" && status != "NEEDS_HUMAN"
}

func retryableTurnStatus(status AgentTurnStatus, recoverySettled bool) bool {
	return status == AgentTurnFailed || status == AgentTurnInterrupted || status == AgentTurnTimedOut ||
		status == AgentTurnReconciling && recoverySettled
}

func terminalAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnSucceeded || status == AgentTurnFailed || status == AgentTurnInterrupted || status == AgentTurnTimedOut
}

func mutationStates(states []MutationState) []string {
	values := make([]string, len(states))
	for index, state := range states {
		values[index] = string(state)
	}
	return values
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

const unsettledMutationsForSessionSQL = `
SELECT EXISTS (
    SELECT 1 FROM tool_invocations AS invocation
    JOIN agent_turns AS turn ON turn.id = invocation.agent_turn_id
    WHERE turn.agent_session_id = $1 AND invocation.kind = 'MUTATION'
    AND invocation.state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
)`

const unsettledMutationsForTurnSQL = `
SELECT EXISTS (
    SELECT 1 FROM tool_invocations WHERE agent_turn_id = $1 AND execution_epoch = $2
    AND kind = 'MUTATION' AND state IN ('RESERVED', 'IN_FLIGHT', 'UNKNOWN', 'RECONCILING')
)`
