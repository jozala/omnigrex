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

const (
	// LabelProvisioningQueue isolates repository label work from Workflow action kinds.
	LabelProvisioningQueue = "label_provisioning"
	// ProvisionManagedLabelsJobKind is the single repository-scoped provisioning kind.
	// It deliberately carries no Workflow scope — provisioning must never start
	// a Workflow — and reuses the generic jobs lifecycle from jobs.go (claim,
	// heartbeat, retry, lease expiry, idempotent inserts) instead of
	// duplicating that machinery. The only workflowless exception to the
	// scope-hierarchy rule is this kind (see isWorkflowlessProvisioningJob);
	// its provenance is always its source webhook delivery.
	ProvisionManagedLabelsJobKind = "PROVISION_MANAGED_LABELS"

	labelProvisioningMaxAttempts = 5
)

// LabelProvisioningRepository is one repository that must receive the managed labels.
type LabelProvisioningRepository struct {
	ID    int64
	Owner string
	Name  string
}

// LabelProvisioningPayload is the immutable per-repository job definition stored
// in the generic jobs table. Only stable installation and repository IDs are
// stored: owner and name are resolved at execution time so a repository rename
// between queueing and execution (including idempotent replay of committed
// batches) can never strand a job on a stale identity.
type LabelProvisioningPayload struct {
	RepositoryID   int64 `json:"repository_id"`
	InstallationID int64 `json:"installation_id"`
}

// MarshalLabelProvisioningPayload encodes one repository provisioning job definition.
func MarshalLabelProvisioningPayload(repository LabelProvisioningRepository, installationID int64) (json.RawMessage, error) {
	return json.Marshal(LabelProvisioningPayload{
		RepositoryID: repository.ID, InstallationID: installationID,
	})
}

// ParseLabelProvisioningPayload decodes and validates one repository provisioning job definition.
func ParseLabelProvisioningPayload(payload json.RawMessage) (LabelProvisioningPayload, error) {
	var parsed LabelProvisioningPayload
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return LabelProvisioningPayload{}, fmt.Errorf("decode label provisioning payload: %w", err)
	}
	if parsed.RepositoryID <= 0 || parsed.InstallationID <= 0 {
		return LabelProvisioningPayload{}, fmt.Errorf("%w: provisioning job repository identity is invalid", ErrLabelProvisioningInvalid)
	}
	return parsed, nil
}

// LabelProvisioningIdempotencyKey scopes one provisioning job to its source
// delivery and repository, so duplicate deliveries and retries converge on one
// row while overlapping events for the same repository remain isolated jobs.
func LabelProvisioningIdempotencyKey(deliveryID string, repositoryID int64) string {
	return fmt.Sprintf("label-provisioning:%s:%d", deliveryID, repositoryID)
}

// labelProvisioningBatchSize bounds one job-insert transaction so a large
// installation never holds the webhook delivery row lock across an
// unbounded batch. The claim is renewed between batches, while no row lock
// is held, so renewal can actually extend the lease.
const labelProvisioningBatchSize = 100

// CompleteLabelProvisioningTransition records per-repository provisioning
// jobs in the generic jobs table and settles its webhook delivery without
// creating a Workflow or normalized event.
//
// Jobs are queued in bounded batches with claim renewal between batches:
// each batch re-locks and re-validates the delivery fence, so a slow
// installation cannot expire the lease mid-insertion and roll back queued
// work. A lost fence aborts with ErrWebhookClaimLost for retry; idempotent
// inserts make retries converge.
//
// Valid repositories always receive durable jobs, even when invalid entries
// are present: with no invalid entries the delivery is marked PROCESSED, and
// with invalid entries the jobs are still queued while the delivery is marked
// FAILED carrying the invalid entries as an observable diagnostic.
func (store *Store) CompleteLabelProvisioningTransition(ctx context.Context, deliveryID, claimToken string, installationID int64, repositories []LabelProvisioningRepository, invalid []string, renewalLease time.Duration) error {
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
	if err := validatePositiveDuration("complete label provisioning renewal lease", renewalLease); err != nil {
		return err
	}

	// Source identity is carried solely by the idempotency key. Provenance
	// must stay empty: there is no normalized_events row for provisioning
	// deliveries (so a normalized_event_id foreign key could never resolve),
	// and sharing one action key across a delivery's repositories would
	// collide on the (normalized_event_id, action_key) uniqueness scope.
	seen := make(map[int64]struct{}, len(repositories))
	deduped := make([]LabelProvisioningRepository, 0, len(repositories))
	for _, repository := range repositories {
		if _, exists := seen[repository.ID]; exists {
			continue
		}
		seen[repository.ID] = struct{}{}
		deduped = append(deduped, repository)
	}

	batches := len(deduped) / labelProvisioningBatchSize
	if len(deduped)%labelProvisioningBatchSize != 0 || len(deduped) == 0 {
		batches++
	}
	for batch := 0; batch < batches; batch++ {
		chunk := deduped[batch*labelProvisioningBatchSize:]
		if len(chunk) > labelProvisioningBatchSize {
			chunk = chunk[:labelProvisioningBatchSize]
		}
		last := batch == batches-1
		if err := store.insertLabelProvisioningBatch(ctx, deliveryID, claimToken, installationID, chunk, last, invalid); err != nil {
			return err
		}
		if !last {
			if err := store.RenewWebhookClaim(ctx, deliveryID, claimToken, renewalLease); err != nil {
				return err
			}
		}
	}
	return nil
}

func (store *Store) insertLabelProvisioningBatch(ctx context.Context, deliveryID, claimToken string, installationID int64, repositories []LabelProvisioningRepository, last bool, invalid []string) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin label provisioning transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Queue the batch's jobs before taking the delivery row lock. Job
	// insertion only touches jobs rows, so a concurrent claim renewal of the
	// delivery row proceeds instead of blocking behind a slow batch. The
	// fence below is re-validated immediately before committing, so a failed
	// fence still rolls the batch back.
	for _, repository := range repositories {
		payload, err := MarshalLabelProvisioningPayload(repository, installationID)
		if err != nil {
			return fmt.Errorf("encode label provisioning job: %w", err)
		}
		if _, err := insertIdempotentJobTx(ctx, tx, jobInsert{
			queue: LabelProvisioningQueue, kind: ProvisionManagedLabelsJobKind, payload: payload,
			maxAttempts:    labelProvisioningMaxAttempts,
			idempotencyKey: LabelProvisioningIdempotencyKey(deliveryID, repository.ID),
		}); err != nil {
			return fmt.Errorf("insert label provisioning job: %w", err)
		}
	}

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

	if last {
		outcome, lastError := WebhookProcessed, ""
		if len(invalid) != 0 {
			outcome = WebhookFailed
			lastError = "label provisioning recorded with invalid entries: " + strings.Join(truncateInvalidEntries(invalid), "; ")
		}
		result, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET status = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claimed_at = NULL,
    lease_expires_at = NULL,
    processed_at = clock_timestamp(),
    last_error = NULLIF($4, '')
WHERE delivery_id = $1
  AND status = 'PROCESSING'
  AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, outcome, lastError)
		if err != nil {
			return fmt.Errorf("complete label provisioning delivery: %w", err)
		}
		if result.RowsAffected() != 1 {
			return ErrWebhookClaimLost
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit label provisioning transition: %w", err)
	}
	return nil
}

func truncateInvalidEntries(invalid []string) []string {
	const maximumReportedEntries = 32
	if len(invalid) <= maximumReportedEntries {
		return invalid
	}
	return append(append([]string{}, invalid[:maximumReportedEntries]...), fmt.Sprintf("and %d more", len(invalid)-maximumReportedEntries))
}

// isWorkflowlessProvisioningJob reports whether a job is repository-scoped
// label provisioning, the only kind allowed without a Workflow scope.
// Its source identity is the idempotency key; provenance stays empty because
// provisioning deliveries have no normalized_events row to reference.
func isWorkflowlessProvisioningJob(job jobInsert) bool {
	return job.kind == ProvisionManagedLabelsJobKind &&
		job.scope.workflowID == "" && job.scope.workflowAttemptID == "" &&
		job.scope.agentAssignmentID == "" && job.scope.agentSessionID == "" &&
		job.scope.agentTurnID == "" && job.scope.executionEpoch == 0 &&
		job.provenance.normalizedEventID == "" &&
		job.provenance.agentTurnSettlementID == "" &&
		job.provenance.workflowInternalEventID == "" &&
		job.provenance.actionKey == ""
}
