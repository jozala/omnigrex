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

// ErrLabelProvisioningInvalid means a provisioning request contradicts durable webhook identity.
var ErrLabelProvisioningInvalid = errors.New("invalid label provisioning request")

// ErrLabelProvisioningLeaseLost means a provisioning operation was attempted without the current lease.
var ErrLabelProvisioningLeaseLost = errors.New("label provisioning lease lost")

const labelProvisioningReclaimLimit = 32

// LabelProvisioningRepository is one repository that must receive the managed labels.
type LabelProvisioningRepository struct {
	ID    int64
	Owner string
	Name  string
}

// LabelProvisioningJob is the current durable state of one repository provisioning unit.
type LabelProvisioningJob struct {
	ID               string
	RepositoryID     int64
	RepositoryOwner  string
	RepositoryName   string
	InstallationID   int64
	SourceDeliveryID string
	Status           string
	Priority         int
	AvailableAt      time.Time
	AttemptCount     int
	MaxAttempts      int
	LeaseOwner       string
	LeaseToken       string
	LeasedAt         *time.Time
	LeaseExpiresAt   *time.Time
	HeartbeatAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	Result           json.RawMessage
	LastError        string
}

// LabelProvisioningLease is the fenced ownership granted for one provisioning attempt.
type LabelProvisioningLease struct {
	LabelProvisioningJob
	Attempt int
}

// CompleteLabelProvisioningTransition atomically records per-repository provisioning work
// and marks its webhook delivery processed without creating a Workflow or normalized event.
func (store *Store) CompleteLabelProvisioningTransition(ctx context.Context, deliveryID, claimToken string, installationID int64, repositories []LabelProvisioningRepository) error {
	if !validUUID(deliveryID) || !validUUID(claimToken) {
		return ErrWebhookClaimLost
	}
	if installationID <= 0 {
		return fmt.Errorf("complete label provisioning: %w: installation identity is invalid", ErrLabelProvisioningInvalid)
	}
	for _, repository := range repositories {
		if repository.ID <= 0 || strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Name) == "" ||
			strings.Contains(repository.Owner, "/") || strings.Contains(repository.Name, "/") {
			return fmt.Errorf("complete label provisioning: %w: repository identity is invalid", ErrLabelProvisioningInvalid)
		}
	}
	seen := make(map[int64]struct{}, len(repositories))
	deduped := make([]LabelProvisioningRepository, 0, len(repositories))
	for _, repository := range repositories {
		if _, exists := seen[repository.ID]; exists {
			continue
		}
		seen[repository.ID] = struct{}{}
		deduped = append(deduped, repository)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin label provisioning transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status WebhookStatus
	var currentToken *string
	var leaseLive bool
	err = tx.QueryRow(ctx, `
SELECT status, claim_token::text, COALESCE(lease_expires_at > clock_timestamp(), FALSE)
FROM webhook_deliveries
WHERE delivery_id = $1
FOR UPDATE`, deliveryID).Scan(&status, &currentToken, &leaseLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWebhookClaimLost
	}
	if err != nil {
		return fmt.Errorf("lock label provisioning delivery: %w", err)
	}
	if status != WebhookProcessing || currentToken == nil || *currentToken != claimToken || !leaseLive {
		return ErrWebhookClaimLost
	}

	for _, repository := range deduped {
		jobID, err := randomUUID()
		if err != nil {
			return fmt.Errorf("generate label provisioning identity: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO repository_label_provisioning (
    id, repository_id, repository_owner, repository_name,
    installation_id, source_delivery_id, status, priority, max_attempts
)
VALUES ($1, $2, $3, $4, $5, $6, 'AVAILABLE', 0, 5)
ON CONFLICT (source_delivery_id, repository_id) DO NOTHING`,
			jobID, repository.ID, repository.Owner, repository.Name,
			installationID, deliveryID); err != nil {
			return fmt.Errorf("insert label provisioning job: %w", err)
		}
	}

	result, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET status = 'PROCESSED',
    claim_owner = NULL,
    claim_token = NULL,
    claimed_at = NULL,
    lease_expires_at = NULL,
    processed_at = clock_timestamp(),
    last_error = NULL
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken)
	if err != nil {
		return fmt.Errorf("complete label provisioning delivery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWebhookClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit label provisioning transition: %w", err)
	}
	return nil
}

// ClaimLabelProvisioningJob leases one available provisioning job without consuming other work.
func (store *Store) ClaimLabelProvisioningJob(ctx context.Context, owner string, lease time.Duration) (*LabelProvisioningLease, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("claim label provisioning job: owner is empty")
	}
	if err := validatePositiveDuration("claim label provisioning job lease", lease); err != nil {
		return nil, err
	}
	token, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("claim label provisioning job: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin label provisioning job claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := reclaimExpiredLabelProvisioningJobsTx(ctx, tx, labelProvisioningReclaimLimit); err != nil {
		return nil, fmt.Errorf("reclaim before label provisioning job claim: %w", err)
	}

	var jobID string
	err = tx.QueryRow(ctx, `
SELECT id::text
FROM repository_label_provisioning
WHERE status = 'AVAILABLE'
  AND available_at <= clock_timestamp()
  AND attempt_count < max_attempts
ORDER BY priority DESC, available_at, id
FOR UPDATE SKIP LOCKED
LIMIT 1`).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty label provisioning job claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claimable label provisioning job: %w", err)
	}

	claimed, err := leaseLabelProvisioningJobTx(ctx, tx, jobID, owner, token, lease)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit label provisioning job claim: %w", err)
	}
	return claimed, nil
}

func leaseLabelProvisioningJobTx(ctx context.Context, tx pgx.Tx, jobID, owner, token string, lease time.Duration) (*LabelProvisioningLease, error) {
	var attempt int
	var leasedAt, expiresAt time.Time
	err := tx.QueryRow(ctx, `
UPDATE repository_label_provisioning
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
		return nil, fmt.Errorf("lease label provisioning job: %w", err)
	}
	row := tx.QueryRow(ctx, labelProvisioningSelect+` WHERE id = $1`, jobID)
	job, err := scanLabelProvisioningJob(row)
	if err != nil {
		return nil, fmt.Errorf("read claimed label provisioning job: %w", err)
	}
	return &LabelProvisioningLease{LabelProvisioningJob: job, Attempt: attempt}, nil
}

// HeartbeatLabelProvisioningJob extends a live provisioning lease.
func (store *Store) HeartbeatLabelProvisioningJob(ctx context.Context, lease LabelProvisioningLease, extension time.Duration) error {
	if err := validatePositiveDuration("heartbeat label provisioning job lease", extension); err != nil {
		return err
	}
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return ErrLabelProvisioningLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin heartbeat label provisioning job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live bool
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED'
   AND lease_token = $2
   AND attempt_count = $3
   AND lease_expires_at > clock_timestamp()
FROM repository_label_provisioning
WHERE id = $1
FOR UPDATE`, lease.ID, lease.LeaseToken, lease.Attempt).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return ErrLabelProvisioningLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock heartbeat label provisioning job: %w", err)
	}
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + $1 * interval '1 microsecond'`, extension.Microseconds()).Scan(&expiresAt); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `
UPDATE repository_label_provisioning
SET heartbeat_at = clock_timestamp(), lease_expires_at = $4, updated_at = clock_timestamp()
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`, lease.ID, lease.LeaseToken, lease.Attempt, expiresAt)
	if err != nil {
		return fmt.Errorf("heartbeat label provisioning job: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrLabelProvisioningLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit heartbeat label provisioning job: %w", err)
	}
	return nil
}

// CompleteLabelProvisioningJob records a successful provisioning result.
func (store *Store) CompleteLabelProvisioningJob(ctx context.Context, lease LabelProvisioningLease, result json.RawMessage) error {
	canonical, err := canonicalJSON(result)
	if err != nil {
		return fmt.Errorf("complete label provisioning job: result: %w", err)
	}
	return store.withLockedLabelProvisioningLease(ctx, lease, "complete label provisioning job", func(tx pgx.Tx) error {
		outcome, err := tx.Exec(ctx, `
UPDATE repository_label_provisioning
SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`,
			lease.ID, lease.LeaseToken, lease.Attempt, canonical)
		if err != nil {
			return err
		}
		if outcome.RowsAffected() != 1 {
			return errors.New("complete label provisioning job row was not updated")
		}
		return nil
	})
}

// FailLabelProvisioningJob records an attempt failure and either schedules a retry or exhausts the job.
func (store *Store) FailLabelProvisioningJob(ctx context.Context, lease LabelProvisioningLease, cause error, retryable bool, retryDelay time.Duration) error {
	if cause == nil {
		return errors.New("fail label provisioning job: cause is nil")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return fmt.Errorf("fail label provisioning job: retry delay must be between zero and %s", maximumJobDelay)
	}
	return store.withLockedLabelProvisioningLease(ctx, lease, "fail label provisioning job", func(tx pgx.Tx) error {
		var maxAttempts int
		if err := tx.QueryRow(ctx, `SELECT max_attempts FROM repository_label_provisioning WHERE id = $1`, lease.ID).Scan(&maxAttempts); err != nil {
			return err
		}
		willRetry := retryable && lease.Attempt < maxAttempts
		outcome, err := tx.Exec(ctx, `
UPDATE repository_label_provisioning
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() + $5 * interval '1 microsecond' ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    last_error = $6
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3`,
			lease.ID, lease.LeaseToken, lease.Attempt, willRetry, retryDelay.Microseconds(), cause.Error())
		if err != nil {
			return err
		}
		if outcome.RowsAffected() != 1 {
			return errors.New("fail label provisioning job row was not updated")
		}
		return nil
	})
}

func reclaimExpiredLabelProvisioningJobsTx(ctx context.Context, tx pgx.Tx, limit int) error {
	rows, err := tx.Query(ctx, `
SELECT id::text, attempt_count, max_attempts, lease_token::text
FROM repository_label_provisioning
WHERE status = 'LEASED'
  AND lease_expires_at <= clock_timestamp()
ORDER BY lease_expires_at, id
FOR UPDATE SKIP LOCKED
LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type expired struct {
		id, token    string
		attempt, max int
	}
	var expiredJobs []expired
	for rows.Next() {
		var job expired
		if err := rows.Scan(&job.id, &job.attempt, &job.max, &job.token); err != nil {
			rows.Close()
			return err
		}
		expiredJobs = append(expiredJobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, job := range expiredJobs {
		retryScheduled := job.attempt < job.max
		if _, err := tx.Exec(ctx, `
UPDATE repository_label_provisioning
SET status = CASE WHEN $4 THEN 'AVAILABLE' ELSE 'FAILED' END,
    available_at = CASE WHEN $4 THEN clock_timestamp() ELSE available_at END,
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = CASE WHEN $4 THEN NULL ELSE clock_timestamp() END,
    last_error = 'lease expired'
WHERE id = $1 AND status = 'LEASED' AND lease_token = $2 AND attempt_count = $3`,
			job.id, job.token, job.attempt, retryScheduled); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) withLockedLabelProvisioningLease(ctx context.Context, lease LabelProvisioningLease, action string, operation func(pgx.Tx) error) error {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return ErrLabelProvisioningLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live bool
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED'
   AND lease_token = $2
   AND attempt_count = $3
   AND lease_expires_at > clock_timestamp()
FROM repository_label_provisioning
WHERE id = $1
FOR UPDATE`, lease.ID, lease.LeaseToken, lease.Attempt).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return ErrLabelProvisioningLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", action, err)
	}
	if err := operation(tx); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", action, err)
	}
	return nil
}

const labelProvisioningSelect = `
SELECT id::text, repository_id, repository_owner, repository_name,
       installation_id, source_delivery_id::text, status, priority,
       available_at, attempt_count, max_attempts,
       COALESCE(lease_owner, ''), COALESCE(lease_token::text, ''), leased_at,
       lease_expires_at, heartbeat_at, created_at, updated_at, completed_at,
       result, COALESCE(last_error, '')
FROM repository_label_provisioning`

func scanLabelProvisioningJob(row rowScanner) (LabelProvisioningJob, error) {
	var job LabelProvisioningJob
	var result []byte
	err := row.Scan(
		&job.ID, &job.RepositoryID, &job.RepositoryOwner, &job.RepositoryName,
		&job.InstallationID, &job.SourceDeliveryID, &job.Status, &job.Priority,
		&job.AvailableAt, &job.AttemptCount, &job.MaxAttempts,
		&job.LeaseOwner, &job.LeaseToken, &job.LeasedAt,
		&job.LeaseExpiresAt, &job.HeartbeatAt, &job.CreatedAt, &job.UpdatedAt,
		&job.CompletedAt, &result, &job.LastError,
	)
	if err != nil {
		return LabelProvisioningJob{}, err
	}
	job.Result = json.RawMessage(result)
	return job, nil
}
