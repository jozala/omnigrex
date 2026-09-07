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
	ErrReviewerActorConflict       = errors.New("reviewer actor identity conflict")
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

	RuntimeLabelAssignmentID = "io.omnigrex.assignment"
	RuntimeLabelSessionID    = "io.omnigrex.agent-session"
	RuntimeLabelTurnID       = "io.omnigrex.agent-turn"
	RuntimeLabelEpoch        = "io.omnigrex.execution-epoch"

	agentTurnSlotsLockID = int64(0x4f4d4e49534c4f54)

	recoveryContinuationPendingInfrastructure = "PENDING_INFRASTRUCTURE_FAILURE"
	recoveryContinuationInfrastructureApplied = "INFRASTRUCTURE_FAILURE_APPLIED"
	recoveryContinuationMutationHandoff       = "MUTATION_RECONCILIATION_HANDOFF_APPLIED"
	recoveryContinuationMigrationHandoff      = "MIGRATION_HANDOFF_APPLIED"
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
	operationLineageID    string
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

type agentTurnJobPayload struct {
	AgentTurnID     string `json:"agent_turn_id"`
	AgentSessionID  string `json:"agent_session_id"`
	ExecutionEpoch  int64  `json:"execution_epoch"`
	ControlRevision int64  `json:"control_revision"`
}

// AgentTurnRepository is the immutable repository and Work Item scope of an acquired Agent Turn.
type AgentTurnRepository struct {
	ID    int64
	Owner string
	Name  string
}

type AgentTurnIssue struct {
	ID     int64
	Number int64
}

// AgentTurnChangeProposal is the active GitHub Pull Request scope of an acquired Agent Turn.
type AgentTurnChangeProposal struct {
	ID                string
	PullRequestID     int64
	PullRequestNumber int64
	BaseRef           string
	BaseSHA           string
	HeadRef           string
	HeadSHA           string
}

// AgentTurnExecutionContext contains only durable launch inputs read under the live turn fence.
type AgentTurnExecutionContext struct {
	WorkflowID     string
	Repository     AgentTurnRepository
	Issue          AgentTurnIssue
	ChangeProposal *AgentTurnChangeProposal
	Assignment     AgentAssignment
	Session        AgentSession
	Turn           AgentTurn
}

// AgentTurnCompletion describes the guarded terminal state of an Agent Turn.
type AgentTurnCompletion struct {
	Status    AgentTurnStatus
	Outcome   json.RawMessage
	LastError string
}

// AgentTurnRecovery is the durable result of fencing execution ownership.
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
	SettlementID            string
	Continuation            string
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

// ReadInvocation is the credential-free result metadata for one synchronous read tool call.
type ReadInvocation struct {
	ToolName   string
	Request    json.RawMessage
	Result     json.RawMessage
	LastError  string
	StartedAt  time.Time
	FinishedAt time.Time
}

type ReadInvocationRecord struct {
	ID               string
	AgentTurnID      string
	ExecutionEpoch   int64
	InvocationNumber int64
	ToolName         string
	Request          json.RawMessage
	Result           json.RawMessage
	Succeeded        bool
	Duration         time.Duration
	StartedAt        time.Time
	FinishedAt       time.Time
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
SELECT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1
AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL)`, hierarchy.workflowID).Scan(&recoveryUnsettled); err != nil {
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
	operationLineageID := turnID
	if spec.RetryOfTurnID != "" {
		var retrySession, retryStatus string
		var retryActive, retryRecoverySettled bool
		err := tx.QueryRow(ctx, `
SELECT agent_session_id::text, status, active, recovery_settled_at IS NOT NULL,
       operation_lineage_id::text
FROM agent_turns WHERE id = $1 FOR UPDATE`, spec.RetryOfTurnID).Scan(
			&retrySession, &retryStatus, &retryActive, &retryRecoverySettled, &operationLineageID,
		)
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
	    retry_of_turn_id, operation_lineage_id, status, active, control_revision, agent_profile_commit_sha,
	    agent_profile_content_sha256, agent_profile_config, purpose, change_proposal_id, expected_head_sha
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', TRUE, $9, $10, $11, $12, $13, $14, $15)
	RETURNING created_at`, turnID, hierarchy.workflowID, hierarchy.sessionID, spec.WorkflowAttemptID, locked.nextTurnNumber,
		locked.nextExecutionEpoch, nullableString(spec.RetryOfTurnID), operationLineageID, spec.ControlRevision,
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
		operationLineageID: operationLineageID,
		TurnNumber:         locked.nextTurnNumber, ExecutionEpoch: locked.nextExecutionEpoch,
		Status: AgentTurnQueued, CreatedAt: createdAt,
	}, nil
}

// ClaimAndAcquireAgentTurn atomically claims the next queued execution job and occupies a global slot.
func (store *Store) ClaimAndAcquireAgentTurn(ctx context.Context, ownerID string, lease time.Duration, concurrencyLimit int) (AgentTurnLease, bool, error) {
	if strings.TrimSpace(ownerID) == "" {
		return AgentTurnLease{}, false, errors.New("claim and acquire agent turn: owner is empty")
	}
	if err := validatePositiveDuration("claim and acquire agent turn lease", lease); err != nil {
		return AgentTurnLease{}, false, err
	}
	if concurrencyLimit <= 0 || concurrencyLimit > 10_000 {
		return AgentTurnLease{}, false, errors.New("claim and acquire agent turn: concurrency limit must be between 1 and 10000")
	}
	jobToken, err := randomUUID()
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("claim and acquire agent turn: %w", err)
	}
	ownerToken, err := randomUUID()
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("claim and acquire agent turn: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("begin atomic agent turn acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnLease{}, false, err
	}
	var occupied int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_turn_slots WHERE lease_expires_at > clock_timestamp()`).Scan(&occupied); err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("count agent turn slots: %w", err)
	}
	if occupied >= concurrencyLimit {
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnLease{}, false, fmt.Errorf("commit capacity-limited agent turn claim: %w", err)
		}
		return AgentTurnLease{}, false, nil
	}

	job, err := scanJob(tx.QueryRow(ctx, jobSelect+`
WHERE queue = $1
  AND kind = $2
  AND status = 'AVAILABLE'
  AND available_at <= clock_timestamp()
  AND attempt_count < max_attempts
ORDER BY priority DESC, available_at, id
FOR UPDATE SKIP LOCKED
LIMIT 1`, AgentTurnQueue, RunAgentTurnJobKind))
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnLease{}, false, fmt.Errorf("commit empty agent turn claim: %w", err)
		}
		return AgentTurnLease{}, false, nil
	}
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("select claimable agent turn job: %w", err)
	}
	payload, err := decodeAgentTurnJobPayload(job.Payload)
	if err != nil || !validAgentTurnJobPayload(payload, job) {
		return AgentTurnLease{}, false, ErrAgentTurnFenceLost
	}
	hierarchy, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false)
	if err != nil {
		return AgentTurnLease{}, false, err
	}
	if hierarchy.controlRevision != payload.ControlRevision {
		return AgentTurnLease{}, false, ErrAgentTurnFenceLost
	}
	turn, err := lockAgentTurn(ctx, tx, job.AgentTurnID)
	if err != nil {
		return AgentTurnLease{}, false, err
	}
	if !hierarchy.allowsAcquisition(turn.AgentTurn) {
		return AgentTurnLease{}, false, ErrAgentTurnHierarchyInactive
	}
	if !turn.active || turn.AgentSessionID != job.AgentSessionID || turn.WorkflowAttemptID != job.WorkflowAttemptID ||
		turn.ExecutionEpoch != job.ExecutionEpoch || turn.ControlRevision != payload.ControlRevision ||
		turn.Status != AgentTurnQueued && turn.Status != AgentTurnStarting ||
		turn.ownerID != "" || turn.ownerToken != "" || turn.leasePresent {
		return AgentTurnLease{}, false, ErrAgentTurnFenceLost
	}

	var attempt int
	var leasedAt, expiresAt time.Time
	err = tx.QueryRow(ctx, `
UPDATE jobs
SET status = 'LEASED', attempt_count = attempt_count + 1,
    lease_owner = $2, lease_token = $3, leased_at = clock_timestamp(),
    lease_expires_at = clock_timestamp() + $4 * interval '1 microsecond',
    heartbeat_at = clock_timestamp(), updated_at = clock_timestamp(),
    completed_at = NULL, result = NULL, last_error = NULL
WHERE id = $1 AND status = 'AVAILABLE' AND attempt_count < max_attempts
RETURNING attempt_count, leased_at, lease_expires_at`, job.ID, ownerID, jobToken, lease.Microseconds()).Scan(
		&attempt, &leasedAt, &expiresAt,
	)
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("lease agent turn job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status, leased_at,
    lease_expires_at, heartbeat_at
)
VALUES ($1, $2, $3, $4, 'LEASED', $5, $6, clock_timestamp())`,
		job.ID, attempt, ownerID, jobToken, leasedAt, expiresAt); err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("record agent turn job attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_turn_slots (
    agent_turn_id, agent_session_id, execution_epoch, control_revision,
    owner_id, owner_token, lease_expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, turn.ID, turn.AgentSessionID, turn.ExecutionEpoch,
		turn.ControlRevision, ownerID, ownerToken, expiresAt); err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("occupy agent turn slot: %w", err)
	}
	result, err := tx.Exec(ctx, `
UPDATE agent_turns
SET status = 'RUNNING', owner_id = $2, owner_token = $3, leased_at = clock_timestamp(),
    lease_expires_at = $4, heartbeat_at = NULL, mutation_admission_open = FALSE,
    mutation_admission_closed_at = NULL, started_at = COALESCE(started_at, clock_timestamp())
WHERE id = $1 AND execution_epoch = $5 AND control_revision = $6 AND active
  AND status IN ('QUEUED', 'STARTING') AND owner_id IS NULL AND owner_token IS NULL`,
		turn.ID, ownerID, ownerToken, expiresAt, turn.ExecutionEpoch, turn.ControlRevision)
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("start claimed agent turn: %w", err)
	}
	if result.RowsAffected() != 1 {
		return AgentTurnLease{}, false, ErrAgentTurnFenceLost
	}
	job, err = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, job.ID))
	if err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("read acquired agent turn job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnLease{}, false, fmt.Errorf("commit atomic agent turn acquisition: %w", err)
	}
	turn.AgentTurn.Status = AgentTurnRunning
	turn.AgentTurn.MutationAdmissionOpen = false
	turn.AgentTurn.AgentAssignmentID = job.AgentAssignmentID
	jobLease := JobLease{Job: job, Attempt: attempt}
	return AgentTurnLease{
		AgentTurn: turn.AgentTurn, JobLease: jobLease, OwnerID: ownerID,
		OwnerToken: ownerToken, LeaseExpiresAt: expiresAt,
	}, true, nil
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
	_, err := store.RefreshAgentTurnLease(ctx, lease, extension)
	return err
}

// RefreshAgentTurnLease atomically extends every execution fence and returns its database-derived expiration.
func (store *Store) RefreshAgentTurnLease(ctx context.Context, lease AgentTurnLease, extension time.Duration) (AgentTurnLease, error) {
	if err := validatePositiveDuration("heartbeat agent turn lease", extension); err != nil {
		return AgentTurnLease{}, err
	}
	var expiresAt time.Time
	err := store.withLockedAgentTurnLeaseStatus(ctx, lease, "heartbeat agent turn", true, func(tx pgx.Tx, _ lockedTurn) error {
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
	if err != nil {
		return AgentTurnLease{}, err
	}
	lease.LeaseExpiresAt = expiresAt
	lease.JobLease.Job.LeaseExpiresAt = &expiresAt
	return lease, nil
}

// BeginAgentTurnRecovery transfers a live, MCP-drained Turn to durable recovery workers.
// A retry with the exact original lease returns the live barrier. After an ambiguous commit that
// cannot be authenticated, callers can use GetAgentTurnRecovery with the Turn identity and epoch.
func (store *Store) BeginAgentTurnRecovery(ctx context.Context, lease AgentTurnLease) (AgentTurnRecovery, error) {
	if !validUUID(lease.ID) || !validUUID(lease.AgentAssignmentID) || !validUUID(lease.AgentSessionID) ||
		!validUUID(lease.JobLease.ID) || !validUUID(lease.JobLease.LeaseToken) || !validUUID(lease.OwnerToken) ||
		lease.ExecutionEpoch <= 0 || lease.ControlRevision <= 0 || lease.JobLease.Attempt <= 0 ||
		strings.TrimSpace(lease.JobLease.LeaseOwner) == "" || strings.TrimSpace(lease.OwnerID) == "" {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin live Agent Turn recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.JobLease.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock live Agent Turn recovery job: %w", err)
	}
	if !sameAgentTurnExecutionIdentity(job, lease) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if job.Status == JobSucceeded {
		recovery, err := lockRepeatedLiveRecovery(ctx, tx, job, lease)
		if err != nil {
			return AgentTurnRecovery{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("commit repeated live Agent Turn mutation recovery: %w", err)
		}
		return recovery, nil
	}
	if job.Status != JobLeased || job.LeaseOwner != lease.JobLease.LeaseOwner ||
		job.LeaseToken != lease.JobLease.LeaseToken || job.AttemptCount != lease.JobLease.Attempt ||
		job.LeaseExpiresAt == nil {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var attemptStatus, attemptOwner, attemptToken string
	var attemptLive bool
	err = tx.QueryRow(ctx, `
SELECT status, lease_owner, lease_token::text, lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.JobLease.Attempt).Scan(&attemptStatus, &attemptOwner, &attemptToken, &attemptLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock live Agent Turn execution attempt: %w", err)
	}
	if attemptStatus != string(JobLeased) || attemptOwner != lease.JobLease.LeaseOwner ||
		attemptToken != lease.JobLease.LeaseToken || !attemptLive {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var jobLive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("check live Agent Turn execution job lease: %w", err)
	}
	if !jobLive {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	hierarchy, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if !hierarchy.allowsActiveTurn() || hierarchy.controlRevision != lease.ControlRevision {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	turn, err := lockAgentTurn(ctx, tx, lease.ID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	turn.AgentAssignmentID = job.AgentAssignmentID
	if !turn.active || turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling || turn.MutationAdmissionOpen || !turn.leaseLive ||
		turn.AgentAssignmentID != lease.AgentAssignmentID || turn.AgentSessionID != lease.AgentSessionID ||
		turn.WorkflowAttemptID != lease.WorkflowAttemptID || turn.ExecutionEpoch != lease.ExecutionEpoch ||
		turn.ControlRevision != lease.ControlRevision || turn.ownerID != lease.OwnerID || turn.ownerToken != lease.OwnerToken {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var admissionClosed, recoveryAbsent bool
	if err := tx.QueryRow(ctx, `
SELECT mutation_admission_closed_at IS NOT NULL,
       recovery_started_at IS NULL AND NOT runtime_stop_required
           AND runtime_stopped_at IS NULL AND recovery_settled_at IS NULL
           AND stop_runtime_job_id IS NULL AND reconcile_mutations_job_id IS NULL
FROM agent_turns WHERE id = $1`, lease.ID).Scan(&admissionClosed, &recoveryAbsent); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("inspect live Agent Turn recovery state: %w", err)
	}
	if !admissionClosed || !recoveryAbsent {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnRecovery{}, err
	}
	var slotLive bool
	err = tx.QueryRow(ctx, `
SELECT lease_expires_at > clock_timestamp() FROM agent_turn_slots
WHERE agent_turn_id = $1 AND agent_session_id = $2 AND execution_epoch = $3
  AND control_revision = $4 AND owner_id = $5 AND owner_token = $6 FOR UPDATE`,
		lease.ID, lease.AgentSessionID, lease.ExecutionEpoch, lease.ControlRevision,
		lease.OwnerID, lease.OwnerToken).Scan(&slotLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock controlled-handoff Agent Turn slot: %w", err)
	}
	if !slotLive {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var recoveryMutations, residualMutations int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state IN ('UNKNOWN', 'RECONCILING', 'SUCCEEDED')),
       count(*) FILTER (WHERE state IN ('RESERVED', 'IN_FLIGHT'))
FROM tool_invocations
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'`,
		lease.ID, lease.ExecutionEpoch).Scan(&recoveryMutations, &residualMutations); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("inspect live Agent Turn recovery mutations: %w", err)
	}
	if residualMutations != 0 {
		return AgentTurnRecovery{}, ErrAgentTurnMutationsUnsettled
	}
	if _, err := tx.Exec(ctx, `
UPDATE tool_invocations SET state = 'RECONCILING', updated_at = clock_timestamp()
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION' AND state = 'UNKNOWN'`,
		lease.ID, lease.ExecutionEpoch); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin live Agent Turn mutation reconciliation: %w", err)
	}
	stopJobID, err := enqueueAgentTurnRecoveryJob(ctx, tx, job, turn, StopStaleRuntimeJobKind, stopStaleRuntimeJobPriority)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	reconcileJobID := ""
	if recoveryMutations != 0 {
		reconcileJobID, err = enqueueAgentTurnRecoveryJob(ctx, tx, job, turn, ReconcileAgentTurnMutationsJobKind, reconcileTurnMutationsJobPriority)
		if err != nil {
			return AgentTurnRecovery{}, err
		}
	}
	recoveryStatus := AgentTurnInterrupted
	if recoveryMutations != 0 {
		recoveryStatus = AgentTurnReconciling
	}
	jobResult := json.RawMessage(`{"controlled_handoff":true}`)
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, jobResult)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("complete controlled-handoff execution attempt: %w", err)
	}
	if attemptResult.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	jobUpdate, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, jobResult)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("complete controlled-handoff execution job: %w", err)
	}
	if jobUpdate.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	slotDelete, err := tx.Exec(ctx, `
DELETE FROM agent_turn_slots
WHERE agent_turn_id = $1 AND agent_session_id = $2 AND execution_epoch = $3
  AND control_revision = $4 AND owner_id = $5 AND owner_token = $6`,
		lease.ID, lease.AgentSessionID, lease.ExecutionEpoch, lease.ControlRevision,
		lease.OwnerID, lease.OwnerToken)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("release controlled-handoff Agent Turn slot: %w", err)
	}
	if slotDelete.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	ownerHash := sha256.Sum256([]byte(lease.OwnerToken))
	turnUpdate, err := tx.Exec(ctx, `
UPDATE agent_turns
SET status = $5, active = FALSE, mutation_admission_open = FALSE,
    owner_id = NULL, owner_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, completed_at = clock_timestamp(), last_error = NULL,
    recovery_started_at = clock_timestamp(), runtime_stop_required = TRUE,
    runtime_stopped_at = NULL, recovery_settled_at = NULL,
    stop_runtime_job_id = $6, reconcile_mutations_job_id = $7,
    recovery_original_owner_id = $8, recovery_original_owner_token_sha256 = $9,
    recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE', recovery_settlement_id = NULL
WHERE id = $1 AND execution_epoch = $2 AND control_revision = $3 AND owner_token = $4
  AND active AND status IN ('SETTLING', 'RECONCILING') AND NOT mutation_admission_open
  AND mutation_admission_closed_at IS NOT NULL AND recovery_started_at IS NULL`,
		lease.ID, lease.ExecutionEpoch, lease.ControlRevision, lease.OwnerToken, recoveryStatus, stopJobID, nullableString(reconcileJobID),
		lease.OwnerID, ownerHash[:])
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("fence controlled-handoff Agent Turn: %w", err)
	}
	if turnUpdate.RowsAffected() != 1 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, lease.ID, lease.ExecutionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("read controlled-handoff recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit live Agent Turn recovery: %w", err)
	}
	return recovery, nil
}

// BeginAgentTurnMutationRecovery preserves the original mutation-specific API.
func (store *Store) BeginAgentTurnMutationRecovery(ctx context.Context, lease AgentTurnLease) (AgentTurnRecovery, error) {
	return store.BeginAgentTurnRecovery(ctx, lease)
}

// RecoverExpiredAgentTurn terminally fences an expired job/turn ownership without reusing its epoch.
// Repeating the exact recovery after an ambiguous commit returns the existing durable barrier.
func (store *Store) RecoverExpiredAgentTurn(ctx context.Context, turnID string, executionEpoch int64) (AgentTurnRecovery, error) {
	if !validUUID(turnID) || executionEpoch <= 0 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin agent turn recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+`
WHERE kind = 'RUN_AGENT_TURN' AND agent_turn_id = $1 AND execution_epoch = $2
FOR UPDATE`, turnID, executionEpoch))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("look up expired agent turn job: %w", err)
	}
	if job.Status != JobLeased {
		recovery, err := readAgentTurnRecovery(ctx, tx, turnID, executionEpoch)
		if err != nil || recovery.JobID != job.ID {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("commit repeated agent turn recovery: %w", err)
		}
		return recovery, nil
	}
	recovery, err := recoverExpiredAgentTurnTx(ctx, tx, job)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit agent turn recovery: %w", err)
	}
	return recovery, nil
}

func recoverExpiredAgentTurnTx(ctx context.Context, tx pgx.Tx, claimedJob Job) (AgentTurnRecovery, error) {
	job, err := lockExpiredAgentTurnJob(ctx, tx, claimedJob.ID, claimedJob.AgentTurnID, claimedJob.ExecutionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	turnID := job.AgentTurnID
	executionEpoch := job.ExecutionEpoch
	if _, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false); err != nil {
		return AgentTurnRecovery{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, turnID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if !turn.active || turn.ExecutionEpoch != executionEpoch || turn.ownerID == "" || turn.ownerToken == "" ||
		turn.Status != AgentTurnQueued && turn.Status != AgentTurnStarting && turn.Status != AgentTurnRunning &&
			turn.Status != AgentTurnSettling && turn.Status != AgentTurnReconciling {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if turn.leaseLive {
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
	var reconciliationRequired bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM tool_invocations
    WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
      AND state IN ('UNKNOWN', 'RECONCILING', 'SUCCEEDED')
)`, turnID, executionEpoch).Scan(&reconciliationRequired); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("check recovery mutations: %w", err)
	}
	status := AgentTurnInterrupted
	if reconciliationRequired {
		status = AgentTurnReconciling
	}
	stopJobID, err := enqueueAgentTurnRecoveryJob(ctx, tx, job, turn, StopStaleRuntimeJobKind, stopStaleRuntimeJobPriority)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	reconcileJobID := ""
	if reconciliationRequired {
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
	ownerHash := sha256.Sum256([]byte(turn.ownerToken))
	var recoveryStartedAt time.Time
	turnResult := tx.QueryRow(ctx, `
UPDATE agent_turns
SET status = $2, active = FALSE, mutation_admission_open = FALSE,
    mutation_admission_closed_at = COALESCE(mutation_admission_closed_at, clock_timestamp()),
    owner_id = NULL, owner_token = NULL, leased_at = NULL, lease_expires_at = NULL,
	    heartbeat_at = NULL, completed_at = clock_timestamp(), last_error = 'execution lease expired',
	    recovery_started_at = clock_timestamp(), runtime_stop_required = TRUE,
	    runtime_stopped_at = NULL, recovery_settled_at = NULL,
	    stop_runtime_job_id = $3, reconcile_mutations_job_id = $4,
	    recovery_original_owner_id = $5, recovery_original_owner_token_sha256 = $6,
	    recovery_continuation = 'PENDING_INFRASTRUCTURE_FAILURE', recovery_settlement_id = NULL
WHERE id = $1 AND active
RETURNING recovery_started_at`, turnID, status, stopJobID, nullableString(reconcileJobID),
		turn.ownerID, ownerHash[:]).Scan(&recoveryStartedAt)
	if err := turnResult; err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("fence expired agent turn: %w", err)
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, turnID, executionEpoch)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("read expired Agent Turn recovery: %w", err)
	}
	if recovery.RecoveryStartedAt == nil || !recovery.RecoveryStartedAt.Equal(recoveryStartedAt) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	return recovery, nil
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
	if _, err := settleAgentTurnRecoveryTx(ctx, tx, job); err != nil {
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

// ReconcileRecoveredMutation records a known terminal result after stale Runtime Process stop is durable.
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
	if err := requireRecoveredRuntimeStopped(ctx, tx, job); err != nil {
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
	if outcome.State == MutationSucceeded {
		if err := bindReviewerActorForSubmitReview(ctx, tx, job.AgentAssignmentID, mutation.ToolName, result); err != nil {
			return MutationReservation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationReservation{}, fmt.Errorf("commit recovered mutation reconciliation: %w", err)
	}
	return mutation, nil
}

// CompleteAgentTurnRecovery verifies that barrier completion already durably continued the Workflow.
func (store *Store) CompleteAgentTurnRecovery(ctx context.Context, turnID string, executionEpoch int64) (AgentTurnRecovery, error) {
	if !validUUID(turnID) || executionEpoch <= 0 {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin Agent Turn recovery completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recovery, err := readAgentTurnRecovery(ctx, tx, turnID, executionEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
		return AgentTurnRecovery{}, fmt.Errorf("read settled Agent Turn recovery: %w", err)
	}
	if recovery.RecoverySettledAt == nil || recovery.Continuation == recoveryContinuationPendingInfrastructure ||
		recovery.Continuation == recoveryContinuationInfrastructureApplied && recovery.SettlementID == "" {
		return AgentTurnRecovery{}, ErrAgentTurnRecoveryUnsettled
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

// WithAgentTurnFence holds the exact live RUNNING Agent Turn authority while operation executes.
// The callback receives the transaction-scoped context and must not call Store methods.
func (store *Store) WithAgentTurnFence(ctx context.Context, lease AgentTurnLease, operation func(context.Context) error) error {
	if operation == nil {
		return errors.New("hold Agent Turn fence: operation is nil")
	}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return store.withLockedAgentTurnLeaseOptions(ctx, lease, "hold Agent Turn fence", true, true, func(_ pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		return operation(operationCtx)
	})
}

// GetAgentTurnExecutionContext returns launch inputs only while the exact acquired epoch remains live.
func (store *Store) GetAgentTurnExecutionContext(ctx context.Context, lease AgentTurnLease) (AgentTurnExecutionContext, error) {
	var execution AgentTurnExecutionContext
	err := store.withLockedAgentTurnLeaseStatus(ctx, lease, "get Agent Turn execution context", true, func(tx pgx.Tx, turn lockedTurn) error {
		if err := tx.QueryRow(ctx, `
SELECT id::text, repository_id, repository_owner, repository_name, issue_id, issue_number
FROM workflows WHERE id = $1`, lease.JobLease.WorkflowID).Scan(
			&execution.WorkflowID, &execution.Repository.ID, &execution.Repository.Owner,
			&execution.Repository.Name, &execution.Issue.ID, &execution.Issue.Number,
		); err != nil {
			return err
		}
		assignment, err := scanAgentAssignment(tx.QueryRow(ctx, agentAssignmentSelect+` WHERE id = $1`, lease.AgentAssignmentID))
		if err != nil {
			return err
		}
		session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, lease.AgentSessionID))
		if err != nil {
			return err
		}
		execution.Assignment = assignment
		execution.Session = session
		execution.Turn = turn.AgentTurn

		if lease.ChangeProposalID == "" {
			return nil
		}
		proposal := &AgentTurnChangeProposal{}
		if err := tx.QueryRow(ctx, `
SELECT id::text, pull_request_id, pull_request_number, base_ref, base_sha, head_ref, head_sha
FROM change_proposals
WHERE id = $1 AND workflow_id = $2 AND repository_id = $3 AND active`,
			lease.ChangeProposalID, execution.WorkflowID, execution.Repository.ID,
		).Scan(&proposal.ID, &proposal.PullRequestID, &proposal.PullRequestNumber,
			&proposal.BaseRef, &proposal.BaseSHA, &proposal.HeadRef, &proposal.HeadSHA); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAgentTurnFenceLost
			}
			return err
		}
		if proposal.HeadSHA != lease.ExpectedHeadSHA {
			return ErrAgentTurnFenceLost
		}
		execution.ChangeProposal = proposal
		return nil
	})
	if err != nil {
		return AgentTurnExecutionContext{}, err
	}
	return execution, nil
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2 || ':' || $3, 0))`, turn.operationLineageID, spec.ToolName, spec.OperationID); err != nil {
			return err
		}
		existing, err := getMutationByOperation(ctx, tx, turn.operationLineageID, spec.ToolName, spec.OperationID)
		if err == nil {
			if existing.AgentTurnID == turn.ID && !sameMutationDefinition(existing, spec) ||
				existing.AgentTurnID != turn.ID && !sameAgentMutationDefinition(existing, spec) {
				return ErrMutationOperationConflict
			}
			if existing.AgentTurnID != turn.ID {
				if err := validateMutationReplayAncestor(ctx, tx, turn.ID, existing.AgentTurnID); err != nil {
					return err
				}
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
	    id, agent_turn_id, execution_epoch, operation_lineage_id, invocation_number, tool_name, kind, state,
	    idempotency_key, operation_id, request, external_service, external_resource_id, expected_sha
)
VALUES ($1, $2, $3, $4, $5, $6, 'MUTATION', 'RESERVED', $7, $7, $8, $9, $10, $11)
RETURNING admitted_at`, invocationID, lease.ID, lease.ExecutionEpoch, turn.operationLineageID, number, spec.ToolName,
			spec.OperationID, spec.Request, nullableString(spec.ExternalService),
			nullableString(spec.ExternalResourceID), nullableString(spec.ExpectedSHA)).Scan(&reservation.AdmittedAt)
	})
	return reservation, err
}

// AcknowledgeMutationReplay projects one validated ancestor result into the current turn ledger.
func (store *Store) AcknowledgeMutationReplay(ctx context.Context, lease AgentTurnLease, sourceID string, spec MutationSpec) error {
	request, err := validateMutationSpec(spec)
	if err != nil {
		return err
	}
	if !validUUID(sourceID) {
		return ErrMutationOperationConflict
	}
	spec.Request = request
	return store.withLockedAgentTurnLease(ctx, lease, "acknowledge mutation replay", func(tx pgx.Tx, turn lockedTurn) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2 || ':' || $3, 0))`, turn.operationLineageID, spec.ToolName, spec.OperationID); err != nil {
			return err
		}
		source, err := getMutationByOperation(ctx, tx, turn.operationLineageID, spec.ToolName, spec.OperationID)
		if err != nil {
			return err
		}
		if source.ID != sourceID || source.AgentTurnID == turn.ID || !sameAgentMutationDefinition(source, spec) ||
			source.State != MutationSucceeded && source.State != MutationFailed {
			return ErrMutationOperationConflict
		}
		if err := validateMutationReplayAncestor(ctx, tx, turn.ID, source.AgentTurnID); err != nil {
			return err
		}
		return recordTerminalMutationReplay(ctx, tx, turn, source)
	})
}

// RecordReadInvocation appends one terminal read-tool record under the live Agent Turn fence.
func (store *Store) RecordReadInvocation(ctx context.Context, lease AgentTurnLease, invocation ReadInvocation) (ReadInvocationRecord, error) {
	if strings.TrimSpace(invocation.ToolName) == "" || invocation.StartedAt.IsZero() || invocation.FinishedAt.Before(invocation.StartedAt) {
		return ReadInvocationRecord{}, errors.New("record read invocation: invalid metadata")
	}
	request, err := canonicalJSON(invocation.Request)
	if err != nil {
		return ReadInvocationRecord{}, fmt.Errorf("record read invocation: request: %w", err)
	}
	succeeded := invocation.LastError == ""
	var result json.RawMessage
	if succeeded {
		result, err = canonicalJSON(invocation.Result)
		if err != nil {
			return ReadInvocationRecord{}, fmt.Errorf("record read invocation: result: %w", err)
		}
	} else if strings.TrimSpace(invocation.LastError) == "" || len(invocation.Result) != 0 {
		return ReadInvocationRecord{}, errors.New("record read invocation: failed read requires only an error")
	}
	id, err := randomUUID()
	if err != nil {
		return ReadInvocationRecord{}, fmt.Errorf("record read invocation: %w", err)
	}
	duration := invocation.FinishedAt.Sub(invocation.StartedAt).Round(time.Millisecond)
	var record ReadInvocationRecord
	err = store.withLockedAgentTurnLease(ctx, lease, "record read invocation", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		var number int64
		if err := tx.QueryRow(ctx, `UPDATE agent_turns SET next_invocation_number = next_invocation_number + 1 WHERE id = $1 RETURNING next_invocation_number - 1`, lease.ID).Scan(&number); err != nil {
			return err
		}
		state := MutationSucceeded
		if !succeeded {
			state = MutationFailed
		}
		_, err := tx.Exec(ctx, `
INSERT INTO tool_invocations (
	    id, agent_turn_id, execution_epoch, operation_lineage_id, invocation_number, tool_name, kind, state,
	    request, result, started_at, finished_at, duration_ms, last_error
)
VALUES ($1, $2, $3, $4, $5, $6, 'READ', $7, $8, $9, $10, $11, $12, NULLIF($13, ''))`,
			id, lease.ID, lease.ExecutionEpoch, turn.operationLineageID, number, invocation.ToolName, state, request,
			nullableJSON(result), invocation.StartedAt, invocation.FinishedAt, duration.Milliseconds(), invocation.LastError)
		if err != nil {
			return err
		}
		record = ReadInvocationRecord{
			ID: id, AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch,
			InvocationNumber: number, ToolName: invocation.ToolName, Request: request,
			Result: result, Succeeded: succeeded, Duration: duration,
			StartedAt: invocation.StartedAt, FinishedAt: invocation.FinishedAt,
		}
		return nil
	})
	return record, err
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

// ListAgentTurnMutationInvocations returns the complete terminal mutation ledger for the exact live turn.
// Mutation admission must already be closed, and any nonterminal invocation rejects the read.
func (store *Store) ListAgentTurnMutationInvocations(ctx context.Context, lease AgentTurnLease) ([]MutationReservation, error) {
	var mutations []MutationReservation
	err := store.withLockedAgentTurnLeaseOptions(ctx, lease, "list Agent Turn mutation invocations", false, true, func(tx pgx.Tx, turn lockedTurn) error {
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
		rows, err := tx.Query(ctx, mutationLedgerSelect, lease.ID, lease.ExecutionEpoch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			mutation, err := scanMutation(rows)
			if err != nil {
				return err
			}
			if mutation.State != MutationSucceeded && mutation.State != MutationFailed {
				return ErrAgentTurnMutationsUnsettled
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

func decodeAgentTurnJobPayload(value json.RawMessage) (agentTurnJobPayload, error) {
	var payload agentTurnJobPayload
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return agentTurnJobPayload{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return agentTurnJobPayload{}, errors.New("agent turn job payload has trailing content")
	}
	return payload, nil
}

func validAgentTurnJobPayload(payload agentTurnJobPayload, job Job) bool {
	return job.Queue == AgentTurnQueue && job.Kind == RunAgentTurnJobKind && job.MaxAttempts == 1 &&
		job.WorkflowID != "" && job.WorkflowAttemptID != "" && job.AgentAssignmentID != "" &&
		job.AgentSessionID != "" && job.AgentTurnID != "" && job.ExecutionEpoch > 0 &&
		payload.AgentTurnID == job.AgentTurnID && payload.AgentSessionID == job.AgentSessionID &&
		payload.ExecutionEpoch == job.ExecutionEpoch && payload.ControlRevision > 0
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
	var attemptOwner string
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_expires_at > clock_timestamp(), lease_owner
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.Attempt, lease.LeaseToken).Scan(&attemptLive, &attemptOwner)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && requireLive && (!attemptLive || job.LeaseExpiresAt == nil) {
		return Job{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return Job{}, fmt.Errorf("lock agent turn job attempt: %w", err)
	}
	if requireLive {
		if lease.LeaseOwner != "" && attemptOwner != lease.LeaseOwner {
			return Job{}, ErrAgentTurnFenceLost
		}
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

func sameAgentTurnExecutionIdentity(job Job, lease AgentTurnLease) bool {
	return job.ID == lease.JobLease.ID && job.Queue == AgentTurnQueue && job.Kind == RunAgentTurnJobKind &&
		job.MaxAttempts == 1 && job.AttemptCount == lease.JobLease.Attempt &&
		job.IdempotencyKey == lease.JobLease.IdempotencyKey && job.WorkflowID == lease.JobLease.WorkflowID &&
		job.WorkflowAttemptID == lease.JobLease.WorkflowAttemptID &&
		job.AgentAssignmentID == lease.JobLease.AgentAssignmentID &&
		job.AgentSessionID == lease.JobLease.AgentSessionID && job.AgentTurnID == lease.JobLease.AgentTurnID &&
		job.ExecutionEpoch == lease.JobLease.ExecutionEpoch && bytes.Equal(job.Payload, lease.JobLease.Payload) &&
		job.WorkflowAttemptID == lease.WorkflowAttemptID && job.AgentAssignmentID == lease.AgentAssignmentID &&
		job.AgentSessionID == lease.AgentSessionID && job.AgentTurnID == lease.ID &&
		job.ExecutionEpoch == lease.ExecutionEpoch
}

func lockRepeatedLiveRecovery(ctx context.Context, tx pgx.Tx, job Job, lease AgentTurnLease) (AgentTurnRecovery, error) {
	if !controlledHandoffResult(job.Result) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var attemptStatus, attemptOwner, attemptToken string
	var attemptResult []byte
	if err := tx.QueryRow(ctx, `
SELECT status, lease_owner, lease_token::text, result
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.JobLease.Attempt).Scan(&attemptStatus, &attemptOwner, &attemptToken, &attemptResult); err != nil ||
		attemptStatus != string(JobSucceeded) || attemptOwner != lease.JobLease.LeaseOwner ||
		attemptToken != lease.JobLease.LeaseToken || !controlledHandoffResult(attemptResult) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if _, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false); err != nil {
		return AgentTurnRecovery{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, lease.ID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if turn.active || turn.Status != AgentTurnInterrupted && turn.Status != AgentTurnReconciling || turn.MutationAdmissionOpen || turn.leasePresent ||
		turn.AgentSessionID != lease.AgentSessionID || turn.WorkflowAttemptID != lease.WorkflowAttemptID ||
		turn.ExecutionEpoch != lease.ExecutionEpoch || turn.ControlRevision != lease.ControlRevision ||
		turn.ownerID != "" || turn.ownerToken != "" {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	recovery, err := readAgentTurnRecovery(ctx, tx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.RecoverySettledAt != nil || recovery.JobID != job.ID ||
		recovery.Status != AgentTurnInterrupted && recovery.Status != AgentTurnReconciling || recovery.StopRuntimeJobID == "" ||
		recovery.Status == AgentTurnReconciling && recovery.ReconcileMutationsJobID == "" ||
		recovery.Status == AgentTurnInterrupted && recovery.ReconcileMutationsJobID != "" {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	var identity recoveryJobIdentity
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM jobs WHERE id = $1 AND kind = $2`,
		recovery.StopRuntimeJobID, StopStaleRuntimeJobKind).Scan(&payload); err != nil ||
		json.Unmarshal(payload, &identity) != nil || identity.WorkflowID != job.WorkflowID ||
		identity.WorkflowAttemptID != job.WorkflowAttemptID || identity.AgentAssignmentID != job.AgentAssignmentID ||
		identity.AgentSessionID != job.AgentSessionID || identity.AgentTurnID != job.AgentTurnID ||
		identity.ExecutionEpoch != job.ExecutionEpoch || identity.ControlRevision != lease.ControlRevision ||
		identity.ExecutionJobID != job.ID || identity.OwnerID != lease.OwnerID ||
		identity.OwnerTokenSHA256 != ownerTokenProof(lease.OwnerToken) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	return recovery, nil
}

func controlledHandoffResult(result json.RawMessage) bool {
	canonical, err := canonicalJSON(result)
	return err == nil && bytes.Equal(canonical, json.RawMessage(`{"controlled_handoff":true}`))
}

func ownerTokenProof(token string) string {
	if token == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
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
	OwnerID           string `json:"owner_id,omitempty"`
	OwnerTokenSHA256  string `json:"owner_token_sha256,omitempty"`
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
		OwnerID: turn.ownerID, OwnerTokenSHA256: ownerTokenProof(turn.ownerToken),
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
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 || strings.TrimSpace(lease.LeaseOwner) == "" {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	if job.Kind != expectedKind || job.Queue != AgentTurnRecoveryQueue || job.Status != JobLeased ||
		job.LeaseOwner != lease.LeaseOwner || job.LeaseToken != lease.LeaseToken || job.AttemptCount != lease.Attempt ||
		job.WorkflowID != lease.WorkflowID || job.WorkflowAttemptID != lease.WorkflowAttemptID ||
		job.AgentAssignmentID != lease.AgentAssignmentID || job.AgentSessionID != lease.AgentSessionID ||
		job.AgentTurnID != lease.AgentTurnID || job.ExecutionEpoch != lease.ExecutionEpoch ||
		!bytes.Equal(job.Payload, lease.Payload) {
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
	var continuation, originalOwnerID, originalOwnerHash string
	if err := tx.QueryRow(ctx, `
SELECT recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL,
       CASE WHEN $3 = 'STOP_STALE_RUNTIME' THEN stop_runtime_job_id::text
             ELSE reconcile_mutations_job_id::text END,
       recovery_continuation, COALESCE(recovery_original_owner_id, ''),
       COALESCE(encode(recovery_original_owner_token_sha256, 'hex'), '')
FROM agent_turns WHERE id = $1 AND execution_epoch = $2`,
		job.AgentTurnID, job.ExecutionEpoch, expectedKind).Scan(
		&recoveryLive, &registeredJobID, &continuation, &originalOwnerID, &originalOwnerHash,
	); err != nil || !recoveryLive || registeredJobID != job.ID {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
	}
	if continuation != recoveryContinuationMigrationHandoff &&
		(identity.OwnerID != originalOwnerID || identity.OwnerTokenSHA256 != originalOwnerHash) {
		return Job{}, lockedTurn{}, ErrAgentTurnRecoveryFenceLost
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
       COALESCE(turn.reconcile_mutations_job_id::text, ''),
       COALESCE(turn.recovery_settlement_id::text, ''), turn.recovery_continuation
FROM agent_turns AS turn
JOIN jobs AS execution_job ON execution_job.agent_turn_id = turn.id
    AND execution_job.execution_epoch = turn.execution_epoch
    AND execution_job.kind = 'RUN_AGENT_TURN'
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND turn.recovery_started_at IS NOT NULL`,
		turnID, epoch).Scan(&recovery.TurnID, &recovery.JobID, &recovery.ExecutionEpoch,
		&recovery.Status, &recovery.RuntimeStopRequired, &recovery.RecoveryStartedAt,
		&recovery.RuntimeStoppedAt, &recovery.RecoverySettledAt,
		&recovery.StopRuntimeJobID, &recovery.ReconcileMutationsJobID,
		&recovery.SettlementID, &recovery.Continuation)
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
	SELECT id::text, agent_session_id::text, workflow_attempt_id::text, operation_lineage_id::text,
	turn_number, execution_epoch, control_revision, status, mutation_admission_open, active,
	COALESCE(owner_id, ''), COALESCE(owner_token::text, ''), lease_expires_at IS NOT NULL,
	COALESCE(lease_expires_at > clock_timestamp(), FALSE), COALESCE(retry_of_turn_id::text, ''),
	COALESCE(purpose, ''), COALESCE(change_proposal_id::text, ''), COALESCE(expected_head_sha, ''),
	agent_profile_commit_sha, agent_profile_content_sha256, agent_profile_config, created_at
FROM agent_turns WHERE id = $1 FOR UPDATE`, turnID).Scan(
		&turn.ID, &turn.AgentSessionID, &turn.WorkflowAttemptID, &turn.operationLineageID, &turn.TurnNumber,
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
	return store.withLockedAgentTurnLeaseOptions(ctx, lease, action, allowCreating, false, operation)
}

func (store *Store) withLockedAgentTurnLeaseOptions(ctx context.Context, lease AgentTurnLease, action string, allowCreating, requireExact bool, operation func(pgx.Tx, lockedTurn) error) error {
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
	if requireExact && (job.LeaseOwner != lease.JobLease.LeaseOwner || !sameAgentTurnExecutionIdentity(job, lease)) {
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
	if requireExact && (turn.AgentAssignmentID != lease.AgentAssignmentID || turn.WorkflowAttemptID != lease.WorkflowAttemptID) {
		return ErrAgentTurnFenceLost
	}
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
	return store.withLockedAgentTurnLease(ctx, lease, action, func(tx pgx.Tx, turn lockedTurn) error {
		resultValue := nullableJSON(result)
		lastError := ""
		if cause != nil {
			lastError = cause.Error()
		}
		var toolName string
		err := tx.QueryRow(ctx, `
UPDATE tool_invocations
SET state = $4, result = $5, last_error = NULLIF($6, ''), updated_at = clock_timestamp(),
finished_at = CASE WHEN $4 IN ('SUCCEEDED', 'FAILED') THEN clock_timestamp() ELSE NULL END,
duration_ms = CASE WHEN $4 IN ('SUCCEEDED', 'FAILED') AND started_at IS NOT NULL
THEN GREATEST(0, (EXTRACT(EPOCH FROM (clock_timestamp() - started_at)) * 1000)::bigint) ELSE NULL END
WHERE id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 AND kind = 'MUTATION' AND state = ANY($7)
RETURNING tool_name`, mutationID, lease.ID, lease.ExecutionEpoch, to, resultValue, lastError, mutationStates(from)).Scan(&toolName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMutationStateConflict
		}
		if err != nil {
			return err
		}
		if to == MutationSucceeded {
			return bindReviewerActorForSubmitReview(ctx, tx, turn.AgentAssignmentID, toolName, result)
		}
		return nil
	})
}

func bindReviewerActorForSubmitReview(ctx context.Context, tx pgx.Tx, assignmentID, toolName string, result json.RawMessage) error {
	if toolName != "submit_review" {
		return nil
	}
	var review struct {
		ActorID int64 `json:"actor_id"`
	}
	if err := json.Unmarshal(result, &review); err != nil || review.ActorID <= 0 {
		return errors.New("bind reviewer actor: submit_review result has no valid actor_id")
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_assignments
SET github_app_actor_id = COALESCE(github_app_actor_id, $2), updated_at = clock_timestamp()
WHERE id = $1 AND role = 'REVIEWER' AND status = 'ACTIVE' AND state_deleted_at IS NULL
  AND (github_app_actor_id IS NULL OR github_app_actor_id = $2)`, assignmentID, review.ActorID)
	if err != nil {
		return fmt.Errorf("bind reviewer actor: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return ErrReviewerActorConflict
	}
	return nil
}

const mutationSelect = `
SELECT id::text, agent_turn_id::text, execution_epoch, invocation_number,
COALESCE(operation_id, ''), tool_name, request, state,
COALESCE(external_service, ''), COALESCE(external_resource_id, ''), COALESCE(expected_sha, ''),
result, COALESCE(last_error, ''), admitted_at, started_at, finished_at
FROM tool_invocations`

const mutationLedgerSelect = `
SELECT id::text, agent_turn_id::text, execution_epoch, invocation_number,
       COALESCE(operation_id, ''), tool_name, request, state,
       COALESCE(external_service, ''), COALESCE(external_resource_id, ''), COALESCE(expected_sha, ''),
       result, COALESCE(last_error, ''), admitted_at, started_at, finished_at
FROM (
    SELECT invocation.id, invocation.agent_turn_id, invocation.execution_epoch,
           invocation.invocation_number, invocation.operation_id, invocation.tool_name,
           invocation.request, invocation.state, invocation.external_service,
           invocation.external_resource_id, invocation.expected_sha, invocation.result,
           invocation.last_error, invocation.admitted_at, invocation.started_at,
           invocation.finished_at
    FROM tool_invocations AS invocation
    WHERE invocation.agent_turn_id = $1 AND invocation.execution_epoch = $2
      AND invocation.kind = 'MUTATION'

    UNION ALL

    SELECT source.id, replay.agent_turn_id, replay.execution_epoch,
           replay.invocation_number, source.operation_id, source.tool_name,
           source.request, source.state, source.external_service,
           source.external_resource_id, source.expected_sha, source.result,
           source.last_error, source.admitted_at, source.started_at,
           source.finished_at
    FROM tool_invocation_replays AS replay
    JOIN tool_invocations AS source
      ON source.id = replay.source_tool_invocation_id
     AND source.agent_turn_id = replay.source_agent_turn_id
     AND source.execution_epoch = replay.source_execution_epoch
     AND source.operation_lineage_id = replay.operation_lineage_id
    WHERE replay.agent_turn_id = $1 AND replay.execution_epoch = $2
) AS invocation
ORDER BY invocation_number`

func recordTerminalMutationReplay(ctx context.Context, tx pgx.Tx, turn lockedTurn, source MutationReservation) error {
	if source.AgentTurnID == turn.ID || source.State != MutationSucceeded && source.State != MutationFailed {
		return nil
	}
	if err := validateMutationReplayAncestor(ctx, tx, turn.ID, source.AgentTurnID); err != nil {
		return err
	}

	var invocationNumber int64
	err := tx.QueryRow(ctx, `
SELECT invocation_number
FROM tool_invocation_replays
WHERE agent_turn_id = $1 AND source_tool_invocation_id = $2`, turn.ID, source.ID).Scan(&invocationNumber)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
		return ErrMutationAdmissionClosed
	}
	if err := tx.QueryRow(ctx, `
UPDATE agent_turns
SET next_invocation_number = next_invocation_number + 1
WHERE id = $1 AND execution_epoch = $2
RETURNING next_invocation_number - 1`, turn.ID, turn.ExecutionEpoch).Scan(&invocationNumber); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO tool_invocation_replays (
    agent_turn_id, execution_epoch, operation_lineage_id, invocation_number,
    source_tool_invocation_id, source_agent_turn_id, source_execution_epoch
)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, turn.ID, turn.ExecutionEpoch,
		turn.operationLineageID, invocationNumber, source.ID, source.AgentTurnID, source.ExecutionEpoch)
	return err
}

func validateMutationReplayAncestor(ctx context.Context, tx pgx.Tx, turnID, sourceTurnID string) error {
	var ancestor bool
	if err := tx.QueryRow(ctx, `
WITH RECURSIVE ancestors (id) AS (
    SELECT retry_of_turn_id FROM agent_turns WHERE id = $1
    UNION ALL
    SELECT candidate.retry_of_turn_id
    FROM agent_turns AS candidate
    JOIN ancestors ON candidate.id = ancestors.id
    WHERE ancestors.id IS NOT NULL
)
SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = $2)`, turnID, sourceTurnID).Scan(&ancestor); err != nil {
		return err
	}
	if !ancestor {
		return ErrMutationOperationConflict
	}
	return nil
}

func getMutationByOperation(ctx context.Context, tx pgx.Tx, operationLineageID, toolName, operationID string) (MutationReservation, error) {
	return scanMutation(tx.QueryRow(ctx, mutationSelect+`
WHERE operation_lineage_id = $1 AND tool_name = $2 AND operation_id = $3 AND kind = 'MUTATION'`, operationLineageID, toolName, operationID))
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

func sameMutationDefinition(existing MutationReservation, spec MutationSpec) bool {
	return sameAgentMutationDefinition(existing, spec) &&
		existing.ExternalService == spec.ExternalService && existing.ExternalResourceID == spec.ExternalResourceID &&
		existing.ExpectedSHA == spec.ExpectedSHA
}

func sameAgentMutationDefinition(existing MutationReservation, spec MutationSpec) bool {
	return existing.OperationID == spec.OperationID && existing.ToolName == spec.ToolName && bytes.Equal(existing.Request, spec.Request)
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
