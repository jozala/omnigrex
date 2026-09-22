package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrJobIdempotencyConflict means a key was reused for a different immutable job definition.
	ErrJobIdempotencyConflict = errors.New("job idempotency conflict")
	// ErrJobLeaseLost means an operation was attempted by a stale or expired job owner.
	ErrJobLeaseLost = errors.New("job lease lost")
	// ErrAgentTurnJobRequiresTurnFence prevents generic lease operations from bypassing turn fencing.
	ErrAgentTurnJobRequiresTurnFence = errors.New("agent turn job requires agent turn fence")
	// ErrWorkflowJobRequiresAcknowledgement prevents generic completion from bypassing workflow fences.
	ErrWorkflowJobRequiresAcknowledgement = errors.New("workflow job requires fenced acknowledgement")
)

const (
	jobClaimReclaimLimit = 32
	maximumJobDelay      = 365 * 24 * time.Hour
)

// JobStatus is the durable lifecycle state of a job.
type JobStatus string

const (
	JobAvailable JobStatus = "AVAILABLE"
	JobLeased    JobStatus = "LEASED"
	JobSucceeded JobStatus = "SUCCEEDED"
	JobFailed    JobStatus = "FAILED"
	JobCancelled JobStatus = "CANCELLED"
)

// JobSpec is the immutable definition of a durable job.
type JobSpec struct {
	Queue              string
	Kind               string
	Payload            json.RawMessage
	Priority           int
	AvailableDelay     time.Duration
	MaxAttempts        int
	IdempotencyKey     string
	WorkflowID         string
	WorkflowAttemptID  string
	AgentParticipantID string
	AgentAssignmentID  string
	AgentSessionID     string
	AgentTurnID        string
	ExecutionEpoch     int64
}

// Job is the current durable state of one job.
type Job struct {
	JobSpec
	ID             string
	Status         JobStatus
	AvailableAt    time.Time
	AttemptCount   int
	LeaseOwner     string
	LeaseToken     string
	LeasedAt       *time.Time
	LeaseExpiresAt *time.Time
	HeartbeatAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	Result         json.RawMessage
	LastError      string
}

// JobLease is the fenced ownership granted for one job attempt.
type JobLease struct {
	Job
	Attempt int
}

// ClaimJobKind leases only jobs of one kind from a queue, without reclaiming or consuming other kinds.
func (store *Store) ClaimJobKind(ctx context.Context, queue, kind, owner string, lease time.Duration) (*JobLease, error) {
	if strings.TrimSpace(kind) == "" {
		return nil, errors.New("claim job: kind is empty")
	}
	if strings.TrimSpace(queue) == "" {
		return nil, errors.New("claim job: queue is empty")
	}
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("claim job: owner is empty")
	}
	if err := validatePositiveDuration("claim job lease", lease); err != nil {
		return nil, err
	}
	token, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin job claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := reclaimExpiredJobsTx(ctx, tx, jobClaimReclaimLimit, queue, kind); err != nil {
		return nil, fmt.Errorf("reclaim before job claim: %w", err)
	}

	var jobID string
	err = tx.QueryRow(ctx, `
SELECT id::text
FROM jobs
WHERE queue = $1
  AND kind = $2
  AND status = 'AVAILABLE'
  AND available_at <= clock_timestamp()
  AND attempt_count < max_attempts
ORDER BY priority DESC, available_at, id
FOR UPDATE SKIP LOCKED
LIMIT 1`, queue, kind).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty job claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claimable job: %w", err)
	}

	claimed, err := leaseJobTx(ctx, tx, jobID, owner, token, lease)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit job claim: %w", err)
	}
	return claimed, nil
}

func leaseJobTx(ctx context.Context, tx pgx.Tx, jobID, owner, token string, lease time.Duration) (*JobLease, error) {
	var attempt int
	var leasedAt, expiresAt time.Time
	err := tx.QueryRow(ctx, `
UPDATE jobs
SET status = 'LEASED',
    attempt_count = attempt_count + 1,
    lease_owner = $2,
    lease_token = $3,
    leased_at = clock_timestamp(),
    lease_expires_at = clock_timestamp() + $4 * interval '1 microsecond',
    heartbeat_at = NULL,
    updated_at = clock_timestamp(),
    completed_at = NULL,
    result = NULL,
    last_error = NULL
WHERE id = $1
  AND status = 'AVAILABLE'
  AND attempt_count < max_attempts
RETURNING attempt_count, leased_at, lease_expires_at`, jobID, owner, token, lease.Microseconds()).Scan(
		&attempt, &leasedAt, &expiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("lease job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO job_attempts (
    job_id, attempt_number, lease_owner, lease_token, status, leased_at, lease_expires_at
)
VALUES ($1, $2, $3, $4, 'LEASED', $5, $6)`, jobID, attempt, owner, token, leasedAt, expiresAt); err != nil {
		return nil, fmt.Errorf("record job attempt: %w", err)
	}
	row := tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID)
	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("read claimed job: %w", err)
	}
	return &JobLease{Job: job, Attempt: attempt}, nil
}

// HeartbeatJob extends a live job and attempt lease held by the supplied fence.
func (store *Store) HeartbeatJob(ctx context.Context, lease JobLease, extension time.Duration) error {
	if err := validatePositiveDuration("heartbeat job lease", extension); err != nil {
		return err
	}
	return store.withLockedJobLease(ctx, lease, "heartbeat job", func(tx pgx.Tx) error {
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + $1 * interval '1 microsecond'`, extension.Microseconds()).Scan(&expiresAt); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `
UPDATE jobs
SET heartbeat_at = clock_timestamp(), lease_expires_at = $4, updated_at = clock_timestamp()
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`, lease.ID, lease.LeaseToken, lease.Attempt, expiresAt)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return errors.New("heartbeat job row was not updated")
		}
		result, err = tx.Exec(ctx, `
UPDATE job_attempts
SET heartbeat_at = clock_timestamp(), lease_expires_at = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
			lease.ID, lease.LeaseToken, lease.Attempt, expiresAt)
		if err == nil && result.RowsAffected() != 1 {
			return errors.New("heartbeat job attempt was not updated")
		}
		return err
	})
}

// CompleteJob atomically records a successful result for a live leased attempt.
func (store *Store) CompleteJob(ctx context.Context, lease JobLease, result json.RawMessage) error {
	canonical, err := canonicalJSON(result)
	if err != nil {
		return fmt.Errorf("complete job: result: %w", err)
	}
	return store.withLockedJobLease(ctx, lease, "complete job", func(tx pgx.Tx) error {
		attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
			lease.ID, lease.LeaseToken, lease.Attempt, canonical)
		if err != nil {
			return err
		}
		if attemptResult.RowsAffected() != 1 {
			return errors.New("complete job attempt was not updated")
		}
		jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`,
			lease.ID, lease.LeaseToken, lease.Attempt, canonical)
		if err == nil && jobResult.RowsAffected() != 1 {
			return errors.New("complete job row was not updated")
		}
		return err
	})
}

// FailJob atomically records an attempt failure and either schedules a retry or exhausts the job.
func (store *Store) FailJob(ctx context.Context, lease JobLease, cause error, retryable bool, retryDelay time.Duration) error {
	if cause == nil {
		return errors.New("fail job: cause is nil")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return fmt.Errorf("fail job: retry delay must be between zero and %s", maximumJobDelay)
	}
	return store.withLockedJobLease(ctx, lease, "fail job", func(tx pgx.Tx) error {
		var maxAttempts int
		if err := tx.QueryRow(ctx, `SELECT max_attempts FROM jobs WHERE id = $1`, lease.ID).Scan(&maxAttempts); err != nil {
			return err
		}
		willRetry := retryable && lease.Attempt < maxAttempts
		attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = $4, last_error = $5
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
			lease.ID, lease.LeaseToken, lease.Attempt, retryable, cause.Error())
		if err != nil {
			return err
		}
		if attemptResult.RowsAffected() != 1 {
			return errors.New("fail job attempt was not updated")
		}
		jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() + $5 * interval '1 microsecond' ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    last_error = $6
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`,
			lease.ID, lease.LeaseToken, lease.Attempt, willRetry, retryDelay.Microseconds(), cause.Error())
		if err == nil && jobResult.RowsAffected() != 1 {
			return errors.New("fail job row was not updated")
		}
		return err
	})
}

func reclaimExpiredJobsTx(ctx context.Context, tx pgx.Tx, limit int, queue, kind string) (int, error) {
	rows, err := tx.Query(ctx, `
SELECT id::text, attempt_count, max_attempts, lease_token::text, kind
FROM jobs
WHERE status = 'LEASED'
  AND kind <> 'RUN_AGENT_TURN'
  AND lease_expires_at <= clock_timestamp()
  AND queue = $2
  AND kind = $3
ORDER BY lease_expires_at, id
FOR UPDATE SKIP LOCKED
LIMIT $1`, limit, queue, kind)
	if err != nil {
		return 0, err
	}
	type expiredJob struct {
		id, token, kind      string
		attempt, maxAttempts int
	}
	var expired []expiredJob
	for rows.Next() {
		var job expiredJob
		if err := rows.Scan(&job.id, &job.attempt, &job.maxAttempts, &job.token, &job.kind); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, job := range expired {
		retryScheduled := job.attempt < job.maxAttempts
		retryDelay := time.Duration(0)
		var expiredAction Job
		if !retryScheduled {
			expiredAction, err = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, job.id))
			if err != nil {
				return 0, err
			}
			continued, _, err := continueIrreversibleJobAfterExhaustionTx(ctx, tx, expiredAction)
			if err != nil {
				return 0, err
			}
			if continued {
				retryScheduled = true
				retryDelay = irreversibleRetryDelay
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'EXPIRED', finished_at = clock_timestamp(), retryable = $4, last_error = 'lease expired'
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`,
			job.id, job.attempt, job.token, retryScheduled); err != nil {
			return 0, err
		}
		if !retryScheduled && isExhaustionAwareWorkflowAction(job.kind) {
			if err := exhaustExpiredWorkflowActionTx(ctx, tx, expiredAction); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE jobs
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() + $5 * interval '1 microsecond' ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    last_error = 'lease expired'
WHERE id = $1 AND status = 'LEASED' AND lease_token = $2 AND attempt_count = $3`,
			job.id, job.token, job.attempt, retryScheduled, retryDelay.Microseconds()); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}

func (store *Store) withLockedJobLease(ctx context.Context, lease JobLease, action string, operation func(pgx.Tx) error) error {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return ErrJobLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind string
	var live bool
	err = tx.QueryRow(ctx, `
SELECT kind,
       status = 'LEASED'
   AND lease_token = $2
   AND attempt_count = $3
   AND lease_expires_at > clock_timestamp()
FROM jobs
WHERE id = $1
FOR UPDATE`, lease.ID, lease.LeaseToken, lease.Attempt).Scan(&kind, &live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return ErrJobLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", action, err)
	}
	if kind == RunAgentTurnJobKind || isAgentTurnRecoveryJob(kind) && action != "heartbeat job" {
		return ErrAgentTurnJobRequiresTurnFence
	}
	if action == "complete job" && isFencedWorkflowJob(kind) {
		return ErrWorkflowJobRequiresAcknowledgement
	}
	if err := operation(tx); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", action, err)
	}
	return nil
}

const jobSelect = `
SELECT id::text, queue, kind, payload, priority, enqueue_delay_microseconds,
       max_attempts, idempotency_key, COALESCE(workflow_id::text, ''),
       COALESCE(workflow_attempt_id::text, ''), COALESCE(agent_assignment_id::text, ''),
       COALESCE(agent_session_id::text, ''), COALESCE(agent_turn_id::text, ''),
       COALESCE(execution_epoch, 0), status, available_at, attempt_count,
       COALESCE(lease_owner, ''), COALESCE(lease_token::text, ''), leased_at,
       lease_expires_at, heartbeat_at, created_at, updated_at, completed_at,
       result, COALESCE(last_error, '')
FROM jobs`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var payload, result []byte
	var delayMicroseconds int64
	err := row.Scan(
		&job.ID, &job.Queue, &job.Kind, &payload, &job.Priority, &delayMicroseconds,
		&job.MaxAttempts, &job.IdempotencyKey, &job.WorkflowID, &job.WorkflowAttemptID,
		&job.AgentAssignmentID, &job.AgentSessionID, &job.AgentTurnID, &job.ExecutionEpoch,
		&job.Status, &job.AvailableAt, &job.AttemptCount, &job.LeaseOwner, &job.LeaseToken,
		&job.LeasedAt, &job.LeaseExpiresAt, &job.HeartbeatAt, &job.CreatedAt, &job.UpdatedAt,
		&job.CompletedAt, &result, &job.LastError,
	)
	if err != nil {
		return Job{}, err
	}
	job.AgentParticipantID = job.AgentAssignmentID
	job.Payload, err = canonicalJSON(payload)
	if err != nil {
		return Job{}, fmt.Errorf("decode job payload: %w", err)
	}
	job.Result = json.RawMessage(result)
	job.AvailableDelay = time.Duration(delayMicroseconds) * time.Microsecond
	return job, nil
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(value) {
		return nil, errors.New("not valid JSON")
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(decoded)
	return json.RawMessage(canonical), err
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableEpoch(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func validatePositiveDuration(name string, value time.Duration) error {
	if value < time.Microsecond || value > maximumJobDelay {
		return fmt.Errorf("%s must be between one microsecond and %s", name, maximumJobDelay)
	}
	return nil
}
