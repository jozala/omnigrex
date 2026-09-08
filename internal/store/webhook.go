package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

// ErrWebhookDeliveryNotFound means the requested durable delivery does not exist.
var ErrWebhookDeliveryNotFound = errors.New("webhook delivery not found")

// ErrWebhookClaimLost means a lease operation was attempted without the current claim token.
var ErrWebhookClaimLost = errors.New("webhook delivery claim lost")

// ErrNormalizedEventNotFound means no normalized transition exists for a delivery.
var ErrNormalizedEventNotFound = errors.New("normalized event not found")

// ErrNormalizedEventDeliveryMismatch means normalized JSON does not identify its delivery row.
var ErrNormalizedEventDeliveryMismatch = errors.New("normalized event delivery ID mismatch")

// WebhookStatus is the durable processing state of a webhook delivery.
type WebhookStatus string

const (
	WebhookPending    WebhookStatus = "PENDING"
	WebhookProcessing WebhookStatus = "PROCESSING"
	WebhookProcessed  WebhookStatus = "PROCESSED"
	WebhookIgnored    WebhookStatus = "IGNORED"
	WebhookFailed     WebhookStatus = "FAILED"
)

// WebhookDelivery is the durable GitHub webhook envelope written to the inbox.
type WebhookDelivery struct {
	DeliveryID      string
	EventName       string
	Action          string
	RepositoryID    int64
	RepositoryOwner string
	RepositoryName  string
	IssueID         int64
	IssueNumber     int64
	Headers         map[string]string
	Payload         []byte
}

// WebhookDeliveryRecord is the observable durable state of one inbox delivery.
type WebhookDeliveryRecord struct {
	WebhookDelivery
	Status         WebhookStatus
	AttemptCount   int
	ClaimOwner     *string
	ClaimToken     *string
	ClaimedAt      *time.Time
	LeaseExpiresAt *time.Time
	ReceivedAt     time.Time
	ProcessedAt    *time.Time
	LastError      *string
}

// WebhookClaim is an exclusively leased delivery returned to a processor.
type WebhookClaim struct {
	WebhookDelivery
	ClaimOwner     string
	ClaimToken     string
	AttemptCount   int
	ClaimedAt      time.Time
	LeaseExpiresAt time.Time
	ReceivedAt     time.Time
}

// WebhookOutcome is the durable result of normalizing a claimed delivery.
type WebhookOutcome string

const (
	WebhookOutcomeProcessed WebhookOutcome = "PROCESSED"
	WebhookOutcomeIgnored   WebhookOutcome = "IGNORED"
)

// WebhookCompletion describes the normalization result committed for a claim.
type WebhookCompletion struct {
	Outcome           WebhookOutcome
	NormalizedPayload json.RawMessage
}

// NormalizedEventStatus is the durable transition queue state.
type NormalizedEventStatus string

const (
	NormalizedEventPending   NormalizedEventStatus = "PENDING"
	NormalizedEventDeferred  NormalizedEventStatus = "DEFERRED"
	NormalizedEventCompleted NormalizedEventStatus = "COMPLETED"
	NormalizedEventFailed    NormalizedEventStatus = "FAILED"
)

// NormalizedEventRecord is a queued normalized transition.
type NormalizedEventRecord struct {
	DeliveryID        string
	Payload           json.RawMessage
	Status            NormalizedEventStatus
	WorkflowID        string
	Disposition       workflow.Disposition
	Reason            workflow.Reason
	AppliedRevision   uint64
	DeferredForTurnID string
	AttemptCount      int
	MaxAttempts       int
	CreatedAt         time.Time
	ProcessedAt       *time.Time
	LastError         *string
}

// InsertWebhookDelivery writes an authenticated delivery once, deduplicated by delivery ID.
func (store *Store) InsertWebhookDelivery(ctx context.Context, delivery WebhookDelivery) (bool, error) {
	headers, err := json.Marshal(delivery.Headers)
	if err != nil {
		return false, fmt.Errorf("encode webhook headers: %w", err)
	}
	result, err := store.pool.Exec(ctx, `
INSERT INTO webhook_deliveries (
    delivery_id, event_name, action, repository_id, repository_owner,
    repository_name, issue_id, issue_number, headers, payload
)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4::BIGINT, 0), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7::BIGINT, 0), NULLIF($8::BIGINT, 0), $9, $10)
ON CONFLICT (delivery_id) DO NOTHING`,
		delivery.DeliveryID,
		delivery.EventName,
		delivery.Action,
		delivery.RepositoryID,
		delivery.RepositoryOwner,
		delivery.RepositoryName,
		delivery.IssueID,
		delivery.IssueNumber,
		headers,
		delivery.Payload,
	)
	if err != nil {
		return false, fmt.Errorf("insert webhook delivery: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// GetWebhookDelivery returns the current durable state of one delivery.
func (store *Store) GetWebhookDelivery(ctx context.Context, deliveryID string) (WebhookDeliveryRecord, error) {
	var record WebhookDeliveryRecord
	var headers []byte
	err := store.pool.QueryRow(ctx, `
SELECT delivery_id::text, event_name, COALESCE(action, ''), COALESCE(repository_id, 0),
       COALESCE(repository_owner, ''), COALESCE(repository_name, ''), COALESCE(issue_id, 0), COALESCE(issue_number, 0), headers, payload, status,
       attempt_count, claim_owner, claim_token::text, claimed_at,
       lease_expires_at, received_at, processed_at, last_error
FROM webhook_deliveries
WHERE delivery_id = $1`, deliveryID).Scan(
		&record.DeliveryID,
		&record.EventName,
		&record.Action,
		&record.RepositoryID,
		&record.RepositoryOwner,
		&record.RepositoryName,
		&record.IssueID,
		&record.IssueNumber,
		&headers,
		&record.Payload,
		&record.Status,
		&record.AttemptCount,
		&record.ClaimOwner,
		&record.ClaimToken,
		&record.ClaimedAt,
		&record.LeaseExpiresAt,
		&record.ReceivedAt,
		&record.ProcessedAt,
		&record.LastError,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookDeliveryRecord{}, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return WebhookDeliveryRecord{}, fmt.Errorf("get webhook delivery: %w", err)
	}
	if err := json.Unmarshal(headers, &record.Headers); err != nil {
		return WebhookDeliveryRecord{}, fmt.Errorf("decode webhook headers: %w", err)
	}
	return record, nil
}

// ClaimWebhookDelivery atomically leases pending or expired deliveries, preferring fresh work over retries.
func (store *Store) ClaimWebhookDelivery(ctx context.Context, owner string, lease time.Duration) (*WebhookClaim, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("claim webhook delivery: owner is empty")
	}
	if lease < time.Microsecond {
		return nil, errors.New("claim webhook delivery: lease must be at least one microsecond")
	}
	claimToken, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("claim webhook delivery: %w", err)
	}

	var claim WebhookClaim
	var headers []byte
	err = store.pool.QueryRow(ctx, `
WITH exhausted AS (
    UPDATE webhook_deliveries
    SET status = 'FAILED',
        claim_owner = NULL,
        claim_token = NULL,
        claimed_at = NULL,
        lease_expires_at = NULL,
        processed_at = clock_timestamp(),
        last_error = COALESCE(last_error, 'webhook delivery attempt limit exhausted')
    WHERE (status = 'PENDING'
       OR (status = 'PROCESSING' AND lease_expires_at <= clock_timestamp()))
      AND attempt_count >= max_attempts
    RETURNING delivery_id
), claimable AS (
    SELECT delivery_id
    FROM webhook_deliveries
    WHERE attempt_count < max_attempts
      AND (status = 'PENDING'
       OR (status = 'PROCESSING' AND lease_expires_at <= clock_timestamp()))
    ORDER BY attempt_count, received_at, delivery_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE webhook_deliveries AS delivery
SET status = 'PROCESSING',
    attempt_count = delivery.attempt_count + 1,
    claim_owner = $1,
    claim_token = $2,
    claimed_at = clock_timestamp(),
    lease_expires_at = clock_timestamp() + $3 * interval '1 microsecond',
    processed_at = NULL,
    last_error = NULL
FROM claimable
WHERE delivery.delivery_id = claimable.delivery_id
RETURNING delivery.delivery_id::text, delivery.event_name, COALESCE(delivery.action, ''),
          COALESCE(delivery.repository_id, 0), COALESCE(delivery.repository_owner, ''), COALESCE(delivery.repository_name, ''),
          COALESCE(delivery.issue_id, 0), COALESCE(delivery.issue_number, 0),
          delivery.headers, delivery.payload, delivery.claim_owner,
	          delivery.claim_token::text, delivery.attempt_count, delivery.claimed_at,
	          delivery.lease_expires_at, delivery.received_at`, owner, claimToken, lease.Microseconds()).Scan(
		&claim.DeliveryID,
		&claim.EventName,
		&claim.Action,
		&claim.RepositoryID,
		&claim.RepositoryOwner,
		&claim.RepositoryName,
		&claim.IssueID,
		&claim.IssueNumber,
		&headers,
		&claim.Payload,
		&claim.ClaimOwner,
		&claim.ClaimToken,
		&claim.AttemptCount,
		&claim.ClaimedAt,
		&claim.LeaseExpiresAt,
		&claim.ReceivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim webhook delivery: %w", err)
	}
	if err := json.Unmarshal(headers, &claim.Headers); err != nil {
		return nil, fmt.Errorf("decode claimed webhook headers: %w", err)
	}
	return &claim, nil
}

// RenewWebhookClaim extends a live lease held by the supplied claim token.
func (store *Store) RenewWebhookClaim(ctx context.Context, deliveryID, claimToken string, lease time.Duration) error {
	if lease < time.Microsecond {
		return errors.New("renew webhook claim: lease must be at least one microsecond")
	}
	if !validUUID(deliveryID) || !validUUID(claimToken) {
		return ErrWebhookClaimLost
	}
	result, err := store.pool.Exec(ctx, `
UPDATE webhook_deliveries
SET lease_expires_at = clock_timestamp() + $3 * interval '1 microsecond'
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, lease.Microseconds())
	if err != nil {
		return fmt.Errorf("renew webhook claim: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWebhookClaimLost
	}
	return nil
}

// CompleteWebhookDelivery atomically records a supported or ignored normalization outcome.
func (store *Store) CompleteWebhookDelivery(ctx context.Context, deliveryID, claimToken string, completion WebhookCompletion) error {
	if !validUUID(deliveryID) || !validUUID(claimToken) {
		return ErrWebhookClaimLost
	}
	switch completion.Outcome {
	case WebhookOutcomeProcessed:
		if !json.Valid(completion.NormalizedPayload) {
			return errors.New("complete webhook delivery: normalized payload is not valid JSON")
		}
		if err := validateNormalizedDeliveryID(completion.NormalizedPayload, deliveryID); err != nil {
			return fmt.Errorf("complete webhook delivery: %w", err)
		}
	case WebhookOutcomeIgnored:
		if len(completion.NormalizedPayload) != 0 {
			return errors.New("complete webhook delivery: ignored outcome has normalized payload")
		}
	default:
		return fmt.Errorf("complete webhook delivery: invalid outcome %q", completion.Outcome)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin webhook completion: %w", err)
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
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWebhookClaimLost
		}
		return fmt.Errorf("lock webhook completion: %w", err)
	}
	if status != WebhookProcessing || currentToken == nil || *currentToken != claimToken || !leaseLive {
		return ErrWebhookClaimLost
	}

	if completion.Outcome == WebhookOutcomeProcessed {
		if _, err := tx.Exec(ctx, `
INSERT INTO normalized_events (delivery_id, payload, status)
VALUES ($1, $2, 'PENDING')
ON CONFLICT (delivery_id) DO NOTHING`, deliveryID, completion.NormalizedPayload); err != nil {
			return fmt.Errorf("insert normalized event: %w", err)
		}
	}
	result, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET status = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claimed_at = NULL,
    lease_expires_at = NULL,
    processed_at = clock_timestamp(),
    last_error = NULL
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, completion.Outcome)
	if err != nil {
		return fmt.Errorf("complete webhook delivery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWebhookClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit webhook completion: %w", err)
	}
	return nil
}

// GetNormalizedEvent returns the normalized transition queued for a delivery.
func (store *Store) GetNormalizedEvent(ctx context.Context, deliveryID string) (NormalizedEventRecord, error) {
	var event NormalizedEventRecord
	err := store.pool.QueryRow(ctx, `
SELECT delivery_id::text, payload, status, COALESCE(workflow_id::text, ''),
       COALESCE(disposition, ''), COALESCE(reason, ''), COALESCE(applied_revision, 0),
       COALESCE(deferred_for_turn_id::text, ''), attempt_count, max_attempts,
       created_at, processed_at, last_error
FROM normalized_events
WHERE delivery_id = $1`, deliveryID).Scan(
		&event.DeliveryID, &event.Payload, &event.Status, &event.WorkflowID,
		&event.Disposition, &event.Reason, &event.AppliedRevision,
		&event.DeferredForTurnID, &event.AttemptCount, &event.MaxAttempts,
		&event.CreatedAt, &event.ProcessedAt, &event.LastError,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return NormalizedEventRecord{}, ErrNormalizedEventNotFound
	}
	if err != nil {
		return NormalizedEventRecord{}, fmt.Errorf("get normalized event: %w", err)
	}
	return event, nil
}

// FailWebhookDelivery records a terminal processing failure held by the supplied claim token.
func (store *Store) FailWebhookDelivery(ctx context.Context, deliveryID, claimToken string, cause error) error {
	if cause == nil {
		return errors.New("fail webhook delivery: cause is nil")
	}
	if !validUUID(deliveryID) || !validUUID(claimToken) {
		return ErrWebhookClaimLost
	}
	result, err := store.pool.Exec(ctx, `
UPDATE webhook_deliveries
SET status = 'FAILED',
    claim_owner = NULL,
    claim_token = NULL,
    claimed_at = NULL,
    lease_expires_at = NULL,
    processed_at = clock_timestamp(),
    last_error = $3
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, cause.Error())
	if err != nil {
		return fmt.Errorf("fail webhook delivery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWebhookClaimLost
	}
	return nil
}

// AcknowledgeWebhookDeliveryFailure releases a fenced claim for retry or terminally fails an exhausted delivery.
func (store *Store) AcknowledgeWebhookDeliveryFailure(ctx context.Context, deliveryID, claimToken string, attemptCount int, cause error, retryable bool) error {
	if cause == nil {
		return errors.New("acknowledge webhook delivery failure: cause is nil")
	}
	if !validUUID(deliveryID) || !validUUID(claimToken) || attemptCount <= 0 {
		return ErrWebhookClaimLost
	}
	result, err := store.pool.Exec(ctx, `
UPDATE webhook_deliveries
SET status = CASE WHEN $5 AND attempt_count < max_attempts THEN 'PENDING' ELSE 'FAILED' END,
    claim_owner = NULL,
    claim_token = NULL,
    claimed_at = NULL,
    lease_expires_at = NULL,
    processed_at = CASE WHEN $5 AND attempt_count < max_attempts THEN NULL ELSE clock_timestamp() END,
    last_error = $4
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND attempt_count = $3
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, attemptCount, cause.Error(), retryable)
	if err != nil {
		return fmt.Errorf("acknowledge webhook delivery failure: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWebhookClaimLost
	}
	return nil
}

func randomUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate claim token: %w", err)
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func validateNormalizedDeliveryID(payload []byte, deliveryID string) error {
	var identity struct {
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal(payload, &identity); err != nil {
		return fmt.Errorf("decode normalized event identity: %w", err)
	}
	if identity.DeliveryID == "" || identity.DeliveryID != deliveryID {
		return ErrNormalizedEventDeliveryMismatch
	}
	return nil
}
