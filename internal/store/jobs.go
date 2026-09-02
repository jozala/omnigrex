package store

import (
	"bytes"
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
	// ErrJobNotFound means the requested durable job does not exist.
	ErrJobNotFound = errors.New("job not found")
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

// JobSpec is the immutable definition used to enqueue a durable job.
type JobSpec struct {
	Queue             string
	Kind              string
	Payload           json.RawMessage
	Priority          int
	AvailableDelay    time.Duration
	MaxAttempts       int
	IdempotencyKey    string
	WorkflowID        string
	WorkflowAttemptID string
	AgentAssignmentID string
	AgentSessionID    string
	AgentTurnID       string
	ExecutionEpoch    int64
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

// EnqueueJob durably inserts a job or returns the identical definition already stored for its key.
func (store *Store) EnqueueJob(ctx context.Context, spec JobSpec) (Job, bool, error) {
	canonicalPayload, err := validateJobSpec(spec)
	if err != nil {
		return Job{}, false, err
	}
	spec.Payload = canonicalPayload
	id, err := randomUUID()
	if err != nil {
		return Job{}, false, fmt.Errorf("enqueue job: %w", err)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Job{}, false, fmt.Errorf("begin enqueue job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := validateJobScope(ctx, tx, spec); err != nil {
		return Job{}, false, err
	}
	result, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, priority, available_at, enqueue_delay_microseconds,
    max_attempts, idempotency_key, workflow_id, workflow_attempt_id,
    agent_assignment_id, agent_session_id, agent_turn_id, execution_epoch
)
VALUES (
    $1, $2, $3, $4, $5, clock_timestamp() + $6 * interval '1 microsecond', $6,
    $7, $8, $9, $10, $11, $12, $13, $14
)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		id, spec.Queue, spec.Kind, spec.Payload, spec.Priority, spec.AvailableDelay.Microseconds(),
		spec.MaxAttempts, spec.IdempotencyKey, nullableString(spec.WorkflowID),
		nullableString(spec.WorkflowAttemptID), nullableString(spec.AgentAssignmentID),
		nullableString(spec.AgentSessionID), nullableString(spec.AgentTurnID), nullableEpoch(spec.ExecutionEpoch),
	)
	if err != nil {
		return Job{}, false, fmt.Errorf("enqueue job: %w", err)
	}
	inserted := result.RowsAffected() == 1
	job, err := getJobByIdempotencyKey(ctx, tx, spec.IdempotencyKey)
	if err != nil {
		return Job{}, false, err
	}
	if !sameJobDefinition(job.JobSpec, spec) {
		return Job{}, false, ErrJobIdempotencyConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, fmt.Errorf("commit enqueue job: %w", err)
	}
	return job, inserted, nil
}

// GetJob returns the current durable state of one job.
func (store *Store) GetJob(ctx context.Context, jobID string) (Job, error) {
	if !validUUID(jobID) {
		return Job{}, ErrJobNotFound
	}
	row := store.pool.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// ClaimJob first reconciles a bounded set of expired attempts, then leases the next available job.
func (store *Store) ClaimJob(ctx context.Context, queue, owner string, lease time.Duration) (*JobLease, error) {
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
	if _, err := reclaimExpiredJobsTx(ctx, tx, jobClaimReclaimLimit, queue); err != nil {
		return nil, fmt.Errorf("reclaim before job claim: %w", err)
	}

	var jobID string
	err = tx.QueryRow(ctx, `
SELECT id::text
FROM jobs
WHERE queue = $1
  AND status = 'AVAILABLE'
  AND available_at <= clock_timestamp()
  AND attempt_count < max_attempts
ORDER BY priority DESC, available_at, id
FOR UPDATE SKIP LOCKED
LIMIT 1`, queue).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty job claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claimable job: %w", err)
	}

	var attempt int
	var leasedAt, expiresAt time.Time
	err = tx.QueryRow(ctx, `
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
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit job claim: %w", err)
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

// ReclaimExpiredJobs settles up to limit expired attempts and makes retryable jobs available.
func (store *Store) ReclaimExpiredJobs(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		return 0, errors.New("reclaim expired jobs: limit must be between 1 and 10000")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin expired job reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	count, err := reclaimExpiredJobsTx(ctx, tx, limit, "")
	if err != nil {
		return 0, fmt.Errorf("reclaim expired jobs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expired job reclaim: %w", err)
	}
	return count, nil
}

func reclaimExpiredJobsTx(ctx context.Context, tx pgx.Tx, limit int, queue string) (int, error) {
	rows, err := tx.Query(ctx, `
SELECT id::text, attempt_count, max_attempts, lease_token::text
FROM jobs
WHERE status = 'LEASED'
  AND kind <> 'RUN_AGENT_TURN'
  AND lease_expires_at <= clock_timestamp()
  AND ($2 = '' OR queue = $2)
ORDER BY lease_expires_at, id
FOR UPDATE SKIP LOCKED
LIMIT $1`, limit, queue)
	if err != nil {
		return 0, err
	}
	type expiredJob struct {
		id, token            string
		attempt, maxAttempts int
	}
	var expired []expiredJob
	for rows.Next() {
		var job expiredJob
		if err := rows.Scan(&job.id, &job.attempt, &job.maxAttempts, &job.token); err != nil {
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
		if _, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'EXPIRED', finished_at = clock_timestamp(), retryable = $4, last_error = 'lease expired'
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`,
			job.id, job.attempt, job.token, job.attempt < job.maxAttempts); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
UPDATE jobs
SET status = CASE WHEN attempt_count < max_attempts THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN attempt_count < max_attempts THEN clock_timestamp() ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN attempt_count < max_attempts THEN NULL ELSE clock_timestamp() END,
    last_error = 'lease expired'
WHERE id = $1 AND status = 'LEASED' AND lease_token = $2 AND attempt_count = $3`,
			job.id, job.token, job.attempt); err != nil {
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

func getJobByIdempotencyKey(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string) (Job, error) {
	job, err := scanJob(queryer.QueryRow(ctx, jobSelect+` WHERE idempotency_key = $1`, key))
	if err != nil {
		return Job{}, fmt.Errorf("read enqueued job: %w", err)
	}
	return job, nil
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
	job.Payload, err = canonicalJSON(payload)
	if err != nil {
		return Job{}, fmt.Errorf("decode job payload: %w", err)
	}
	job.Result = json.RawMessage(result)
	job.AvailableDelay = time.Duration(delayMicroseconds) * time.Microsecond
	return job, nil
}

func validateJobSpec(spec JobSpec) (json.RawMessage, error) {
	if strings.TrimSpace(spec.Queue) == "" || strings.TrimSpace(spec.Kind) == "" {
		return nil, errors.New("enqueue job: queue and kind are required")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return nil, errors.New("enqueue job: idempotency key is required")
	}
	if spec.Kind == RunAgentTurnJobKind || spec.Kind == PrepareAgentTurnJobKind || isAgentTurnRecoveryJob(spec.Kind) {
		return nil, errors.New("enqueue job: Agent Turn control job kind is reserved")
	}
	if spec.MaxAttempts <= 0 {
		return nil, errors.New("enqueue job: max attempts must be positive")
	}
	if spec.AvailableDelay < 0 || spec.AvailableDelay > maximumJobDelay {
		return nil, fmt.Errorf("enqueue job: available delay must be between zero and %s", maximumJobDelay)
	}
	ids := []string{spec.WorkflowID, spec.WorkflowAttemptID, spec.AgentAssignmentID, spec.AgentSessionID, spec.AgentTurnID}
	for _, id := range ids {
		if id != "" && !validUUID(id) {
			return nil, errors.New("enqueue job: scope contains an invalid UUID")
		}
	}
	if spec.WorkflowAttemptID != "" && spec.WorkflowID == "" ||
		spec.AgentAssignmentID != "" && spec.WorkflowID == "" ||
		spec.AgentSessionID != "" && spec.AgentAssignmentID == "" ||
		spec.AgentTurnID != "" && spec.AgentSessionID == "" ||
		(spec.AgentTurnID == "") != (spec.ExecutionEpoch == 0) {
		return nil, errors.New("enqueue job: scope hierarchy is incomplete")
	}
	if spec.ExecutionEpoch < 0 {
		return nil, errors.New("enqueue job: execution epoch must be positive")
	}
	payload, err := canonicalJSON(spec.Payload)
	if err != nil {
		return nil, fmt.Errorf("enqueue job: payload: %w", err)
	}
	return payload, nil
}

func validateJobScope(ctx context.Context, tx pgx.Tx, spec JobSpec) error {
	checks := []struct {
		present bool
		query   string
		args    []any
	}{
		{spec.WorkflowID != "", `SELECT 1 FROM workflows WHERE id = $1 FOR KEY SHARE`, []any{spec.WorkflowID}},
		{spec.WorkflowAttemptID != "", `SELECT 1 FROM workflow_attempts WHERE id = $1 AND workflow_id = $2 FOR KEY SHARE`, []any{spec.WorkflowAttemptID, spec.WorkflowID}},
		{spec.AgentAssignmentID != "", `SELECT 1 FROM agent_assignments WHERE id = $1 AND workflow_id = $2 FOR KEY SHARE`, []any{spec.AgentAssignmentID, spec.WorkflowID}},
		{spec.AgentSessionID != "", `SELECT 1 FROM agent_sessions WHERE id = $1 AND agent_assignment_id = $2 FOR KEY SHARE`, []any{spec.AgentSessionID, spec.AgentAssignmentID}},
		{spec.AgentTurnID != "", `SELECT 1 FROM agent_turns WHERE id = $1 AND agent_session_id = $2 AND execution_epoch = $3 FOR KEY SHARE`, []any{spec.AgentTurnID, spec.AgentSessionID, spec.ExecutionEpoch}},
	}
	for _, check := range checks {
		if !check.present {
			continue
		}
		var ignored int
		if err := tx.QueryRow(ctx, check.query, check.args...).Scan(&ignored); errors.Is(err, pgx.ErrNoRows) {
			return errors.New("enqueue job: scope identities are inconsistent")
		} else if err != nil {
			return fmt.Errorf("validate job scope: %w", err)
		}
	}
	return nil
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

func sameJobDefinition(left, right JobSpec) bool {
	return left.Queue == right.Queue && left.Kind == right.Kind && bytes.Equal(left.Payload, right.Payload) &&
		left.Priority == right.Priority && left.AvailableDelay == right.AvailableDelay &&
		left.MaxAttempts == right.MaxAttempts && left.IdempotencyKey == right.IdempotencyKey &&
		left.WorkflowID == right.WorkflowID && left.WorkflowAttemptID == right.WorkflowAttemptID &&
		left.AgentAssignmentID == right.AgentAssignmentID && left.AgentSessionID == right.AgentSessionID &&
		left.AgentTurnID == right.AgentTurnID && left.ExecutionEpoch == right.ExecutionEpoch
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
