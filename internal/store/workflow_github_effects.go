package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	ReconcileGitHubLabelsJobKind = "RECONCILE_GITHUB_LABELS"
	PublishHumanHandoffJobKind   = "PUBLISH_HUMAN_HANDOFF"
)

var ErrWorkflowGitHubEffectFenceLost = errors.New("Workflow GitHub effect fence lost")

// WorkflowGitHubChangeProposal is the active Change Proposal relevant to a visible GitHub effect.
type WorkflowGitHubChangeProposal struct {
	ID          int64
	Number      int64
	HeadSHA     string
	ReadyForSHA string
}

// WorkflowGitHubEffectContext is the latest durable Workflow state observed under a live job fence.
type WorkflowGitHubEffectContext struct {
	WorkflowID               string
	JobRevision              uint64
	RepositoryID             int64
	RepositoryOwner          string
	RepositoryName           string
	IssueNumber              int64
	State                    workflow.State
	Revision                 uint64
	ReadyForSHA              string
	HandoffReason            string
	HandoffDiagnostic        string
	ChangeProposal           *WorkflowGitHubChangeProposal
	CleanupRequired          bool
	CleanupIssueNumber       int64
	CleanupPullRequestNumber int64
}

// WorkflowGitHubEffectAcknowledgement records whether an effect still represented current Workflow state.
type WorkflowGitHubEffectAcknowledgement struct {
	JobID           string
	WorkflowID      string
	Revision        uint64
	State           workflow.State
	Superseded      bool
	RetryScheduled  bool
	CleanupRequired bool
	HandoffApplied  bool
}

type workflowGitHubEffectPayload struct {
	Revision          int64 `json:"revision"`
	PullRequestNumber int64 `json:"pull_request_number"`
}

// ClaimWorkflowGitHubEffectJob leases one exact visible-effect kind while serializing claims per Workflow.
func (store *Store) ClaimWorkflowGitHubEffectJob(ctx context.Context, kind, owner string, lease time.Duration) (*JobLease, error) {
	if _, ok := workflowGitHubEffectKind(kind); !ok {
		return nil, errors.New("claim Workflow GitHub effect job: invalid kind")
	}
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("claim Workflow GitHub effect job: owner is empty")
	}
	if err := validatePositiveDuration("claim Workflow GitHub effect job lease", lease); err != nil {
		return nil, err
	}
	token, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("claim Workflow GitHub effect job: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin Workflow GitHub effect job claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := reclaimExpiredJobsTx(ctx, tx, jobClaimReclaimLimit, WorkflowActionQueue, kind); err != nil {
		return nil, fmt.Errorf("reclaim before Workflow GitHub effect job claim: %w", err)
	}

	var jobID string
	err = tx.QueryRow(ctx, `
SELECT candidate.id::text
FROM jobs AS candidate
JOIN workflows AS workflow ON workflow.id = candidate.workflow_id
WHERE candidate.queue = $1
  AND candidate.kind = $2
  AND candidate.status = 'AVAILABLE'
  AND candidate.available_at <= clock_timestamp()
  AND candidate.attempt_count < candidate.max_attempts
  AND NOT EXISTS (
      SELECT 1
      FROM jobs AS live
      WHERE live.workflow_id = candidate.workflow_id
        AND live.kind = candidate.kind
        AND live.status = 'LEASED'
        AND live.lease_expires_at > clock_timestamp()
  )
ORDER BY candidate.priority DESC, candidate.available_at, candidate.id
FOR UPDATE OF candidate, workflow SKIP LOCKED
LIMIT 1`, WorkflowActionQueue, kind).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty Workflow GitHub effect job claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claimable Workflow GitHub effect job: %w", err)
	}
	claimed, err := leaseJobTx(ctx, tx, jobID, owner, token, lease)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Workflow GitHub effect job claim: %w", err)
	}
	return claimed, nil
}

// GetWorkflowGitHubEffectContext reads current visible-effect inputs while proving the job lease is live.
func (store *Store) GetWorkflowGitHubEffectContext(ctx context.Context, lease JobLease) (WorkflowGitHubEffectContext, error) {
	expectedKind, ok := workflowGitHubEffectKind(lease.Kind)
	if !ok {
		return WorkflowGitHubEffectContext{}, ErrWorkflowGitHubEffectFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowGitHubEffectContext{}, fmt.Errorf("begin Workflow GitHub effect context read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := lockFencedWorkflowJob(ctx, tx, lease, expectedKind, ErrWorkflowGitHubEffectFenceLost)
	if err != nil {
		return WorkflowGitHubEffectContext{}, err
	}
	var payload workflowGitHubEffectPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.Revision <= 0 {
		return WorkflowGitHubEffectContext{}, ErrWorkflowGitHubEffectFenceLost
	}

	effect := WorkflowGitHubEffectContext{WorkflowID: job.WorkflowID, JobRevision: uint64(payload.Revision)}
	err = tx.QueryRow(ctx, `
SELECT repository_id, repository_owner, repository_name, issue_number,
	   status, state_revision, COALESCE(human_handoff_reason, '')
FROM workflows WHERE id = $1 FOR SHARE`, job.WorkflowID).Scan(
		&effect.RepositoryID, &effect.RepositoryOwner, &effect.RepositoryName,
		&effect.IssueNumber, &effect.State, &effect.Revision, &effect.HandoffReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowGitHubEffectContext{}, ErrWorkflowGitHubEffectFenceLost
	}
	if err != nil {
		return WorkflowGitHubEffectContext{}, fmt.Errorf("read Workflow GitHub effect context: %w", err)
	}

	var proposal WorkflowGitHubChangeProposal
	err = tx.QueryRow(ctx, `
SELECT pull_request_id, pull_request_number, head_sha, COALESCE(ready_for_sha, '')
FROM change_proposals
WHERE workflow_id = $1 AND repository_id = $2 AND active
FOR SHARE`, job.WorkflowID, effect.RepositoryID).Scan(
		&proposal.ID, &proposal.Number, &proposal.HeadSHA, &proposal.ReadyForSHA,
	)
	if err == nil {
		effect.ChangeProposal = &proposal
		effect.ReadyForSHA = proposal.ReadyForSHA
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return WorkflowGitHubEffectContext{}, fmt.Errorf("read active Change Proposal for GitHub effect: %w", err)
	}

	if effect.HandoffReason != "" {
		err = tx.QueryRow(ctx, `
SELECT COALESCE(payload->>'diagnostic', '')
FROM jobs
WHERE workflow_id = $1 AND kind = $2
  AND payload->>'revision' = $3
  AND payload->>'reason' = $4
ORDER BY created_at DESC, id DESC
LIMIT 1`, job.WorkflowID, PublishHumanHandoffJobKind, fmt.Sprint(effect.Revision), effect.HandoffReason).Scan(&effect.HandoffDiagnostic)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return WorkflowGitHubEffectContext{}, fmt.Errorf("read current Human Handoff diagnostic: %w", err)
		}
	}
	var cleanupIssueNumber, cleanupPullRequestNumber int64
	err = tx.QueryRow(ctx, `
SELECT issue_number, COALESCE(pull_request_number, 0)
FROM workflow_github_effect_cleanups
WHERE job_id = $1 AND status = 'PENDING'`, job.ID).Scan(&cleanupIssueNumber, &cleanupPullRequestNumber)
	if err == nil {
		effect.CleanupRequired = true
		effect.CleanupIssueNumber = cleanupIssueNumber
		effect.CleanupPullRequestNumber = cleanupPullRequestNumber
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return WorkflowGitHubEffectContext{}, fmt.Errorf("read pending Workflow GitHub effect cleanup: %w", err)
	}
	if err := validateWorkflowGitHubEffectContext(effect); err != nil {
		return WorkflowGitHubEffectContext{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowGitHubEffectContext{}, fmt.Errorf("commit Workflow GitHub effect context read: %w", err)
	}
	return effect, nil
}

// AcknowledgeWorkflowGitHubEffect completes a visible-effect job and detects state changes since its remote calls began.
func (store *Store) AcknowledgeWorkflowGitHubEffect(ctx context.Context, lease JobLease, observed WorkflowGitHubEffectContext, result json.RawMessage) (WorkflowGitHubEffectAcknowledgement, error) {
	return store.acknowledgeWorkflowGitHubEffect(ctx, lease, observed, result, nil, false, 0)
}

// AcknowledgeWorkflowGitHubEffectFailure fails or retries a visible-effect job unless newer Workflow state superseded it.
func (store *Store) AcknowledgeWorkflowGitHubEffectFailure(ctx context.Context, lease JobLease, observed WorkflowGitHubEffectContext, cause error, retryable bool, retryDelay time.Duration) (WorkflowGitHubEffectAcknowledgement, error) {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return WorkflowGitHubEffectAcknowledgement{}, errors.New("acknowledge Workflow GitHub effect failure: cause is empty")
	}
	if retryDelay < 0 || retryDelay > maximumJobDelay {
		return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("acknowledge Workflow GitHub effect failure: retry delay must be between zero and %s", maximumJobDelay)
	}
	return store.acknowledgeWorkflowGitHubEffect(ctx, lease, observed, nil, cause, retryable, retryDelay)
}

func (store *Store) acknowledgeWorkflowGitHubEffect(ctx context.Context, lease JobLease, observed WorkflowGitHubEffectContext, result json.RawMessage, cause error, retryable bool, retryDelay time.Duration) (WorkflowGitHubEffectAcknowledgement, error) {
	expectedKind, ok := workflowGitHubEffectKind(lease.Kind)
	if !ok || observed.WorkflowID != lease.WorkflowID || observed.Revision == 0 || observed.State == "" {
		return WorkflowGitHubEffectAcknowledgement{}, ErrWorkflowGitHubEffectFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("begin Workflow GitHub effect acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockFencedWorkflowJob(ctx, tx, lease, expectedKind, ErrWorkflowGitHubEffectFenceLost)
	if err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, err
	}
	var payload workflowGitHubEffectPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.Revision <= 0 || observed.JobRevision != uint64(payload.Revision) {
		return WorkflowGitHubEffectAcknowledgement{}, ErrWorkflowGitHubEffectFenceLost
	}
	var currentRevision uint64
	var currentState workflow.State
	var repositoryID, issueNumber int64
	var repositoryOwner, repositoryName string
	if err := tx.QueryRow(ctx, `
SELECT state_revision, status, repository_id, repository_owner, repository_name, issue_number
FROM workflows WHERE id = $1 FOR UPDATE`, job.WorkflowID).Scan(
		&currentRevision, &currentState, &repositoryID, &repositoryOwner, &repositoryName, &issueNumber,
	); err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, ErrWorkflowGitHubEffectFenceLost
	}
	if observed.RepositoryID != repositoryID || observed.RepositoryOwner != repositoryOwner ||
		observed.RepositoryName != repositoryName || observed.IssueNumber != issueNumber {
		return WorkflowGitHubEffectAcknowledgement{}, ErrWorkflowGitHubEffectFenceLost
	}

	acknowledgement := WorkflowGitHubEffectAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, Revision: currentRevision, State: currentState,
		Superseded: currentRevision != observed.Revision || currentState != observed.State,
	}
	cleanupPending := false
	if expectedKind == PublishHumanHandoffJobKind {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
    SELECT 1 FROM workflow_github_effect_cleanups WHERE job_id = $1 AND status = 'PENDING'
)`, job.ID).Scan(&cleanupPending); err != nil {
			return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("inspect Workflow GitHub effect cleanup: %w", err)
		}
	}
	sourceSuperseded := acknowledgement.Superseded || expectedKind == PublishHumanHandoffJobKind &&
		(uint64(payload.Revision) != currentRevision || currentState != workflow.StateNeedsHuman)
	if cleanupPending && cause == nil {
		acknowledgement.Superseded = true
		acknowledgement.CleanupRequired = true
	} else if !cleanupPending && sourceSuperseded && expectedKind == PublishHumanHandoffJobKind {
		pullRequestNumber := payload.PullRequestNumber
		if pullRequestNumber == 0 && observed.ChangeProposal != nil {
			pullRequestNumber = observed.ChangeProposal.Number
		}
		if err := recordWorkflowGitHubEffectCleanupTx(ctx, tx, job, observed, currentRevision, currentState, pullRequestNumber); err != nil {
			return WorkflowGitHubEffectAcknowledgement{}, err
		}
		acknowledgement.Superseded = true
		acknowledgement.CleanupRequired = true
	} else if acknowledgement.Superseded && !cleanupPending {
		supersededResult, _ := json.Marshal(map[string]any{
			"superseded": true, "observed_revision": observed.Revision,
			"current_revision": currentRevision, "current_state": currentState,
		})
		if err := completeAcknowledgementJobTx(ctx, tx, job, supersededResult, ErrWorkflowGitHubEffectFenceLost); err != nil {
			return WorkflowGitHubEffectAcknowledgement{}, err
		}
	} else if cause == nil {
		if err := completeAcknowledgementJobTx(ctx, tx, job, result, ErrWorkflowGitHubEffectFenceLost); err != nil {
			return WorkflowGitHubEffectAcknowledgement{}, err
		}
	} else {
		acknowledgement.RetryScheduled = retryable && job.AttemptCount < job.MaxAttempts
		if !acknowledgement.RetryScheduled {
			if cleanupPending {
				if err := exhaustWorkflowGitHubEffectCleanupTx(ctx, tx, job, cause.Error()); err != nil {
					return WorkflowGitHubEffectAcknowledgement{}, err
				}
				acknowledgement.Superseded = true
				acknowledgement.CleanupRequired = false
			}
			if _, err := requestWorkflowActionFailureEscalationTx(ctx, tx, job, "", cause.Error()); err != nil {
				return WorkflowGitHubEffectAcknowledgement{}, err
			}
		}
		if err := failWorkflowActionJobTx(ctx, tx, job, cause.Error(), retryable, acknowledgement.RetryScheduled, retryDelay); err != nil {
			return WorkflowGitHubEffectAcknowledgement{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("commit Workflow GitHub effect acknowledgement: %w", err)
	}
	return acknowledgement, nil
}

// AcknowledgeWorkflowGitHubEffectCleanup completes a superseded handoff job only after marker cleanup was verified.
func (store *Store) AcknowledgeWorkflowGitHubEffectCleanup(ctx context.Context, lease JobLease) (WorkflowGitHubEffectAcknowledgement, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("begin Workflow GitHub effect cleanup acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockFencedWorkflowJob(ctx, tx, lease, PublishHumanHandoffJobKind, ErrWorkflowGitHubEffectFenceLost)
	if err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, err
	}
	var observedRevision, currentRevision uint64
	var observedState, currentState workflow.State
	if err := tx.QueryRow(ctx, `
UPDATE workflow_github_effect_cleanups
SET status = 'SUCCEEDED', cleaned_at = clock_timestamp(), last_error = NULL
WHERE job_id = $1 AND workflow_id = $2 AND status = 'PENDING'
RETURNING observed_revision, observed_state, current_revision, current_state`, job.ID, job.WorkflowID).Scan(
		&observedRevision, &observedState, &currentRevision, &currentState,
	); err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, ErrWorkflowGitHubEffectFenceLost
	}
	jobResult, _ := json.Marshal(map[string]any{
		"superseded": true, "cleanup_verified": true,
		"observed_revision": observedRevision, "current_revision": currentRevision,
		"current_state": currentState,
	})
	if err := completeAcknowledgementJobTx(ctx, tx, job, jobResult, ErrWorkflowGitHubEffectFenceLost); err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowGitHubEffectAcknowledgement{}, fmt.Errorf("commit Workflow GitHub effect cleanup acknowledgement: %w", err)
	}
	return WorkflowGitHubEffectAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, Revision: currentRevision, State: currentState, Superseded: true,
	}, nil
}

func exhaustWorkflowGitHubEffectCleanupTx(ctx context.Context, tx pgx.Tx, job Job, diagnostic string) error {
	result, err := tx.Exec(ctx, `
UPDATE workflow_github_effect_cleanups
SET status = 'EXHAUSTED', last_error = $3, exhausted_at = clock_timestamp()
WHERE job_id = $1 AND workflow_id = $2 AND status = 'PENDING'`, job.ID, job.WorkflowID, diagnostic)
	if err != nil {
		return fmt.Errorf("exhaust Workflow GitHub effect cleanup: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWorkflowGitHubEffectFenceLost
	}
	return nil
}

func recordWorkflowGitHubEffectCleanupTx(ctx context.Context, tx pgx.Tx, job Job, observed WorkflowGitHubEffectContext, currentRevision uint64, currentState workflow.State, pullRequestNumber int64) error {
	result, err := tx.Exec(ctx, `
INSERT INTO workflow_github_effect_cleanups (
    job_id, workflow_id, issue_number, pull_request_number,
    observed_revision, observed_state, current_revision, current_state
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (job_id) DO UPDATE SET
    current_revision = EXCLUDED.current_revision,
    current_state = EXCLUDED.current_state
WHERE workflow_github_effect_cleanups.workflow_id = EXCLUDED.workflow_id
  AND workflow_github_effect_cleanups.issue_number = EXCLUDED.issue_number
  AND COALESCE(workflow_github_effect_cleanups.pull_request_number, 0) = COALESCE(EXCLUDED.pull_request_number, 0)
  AND workflow_github_effect_cleanups.observed_revision = EXCLUDED.observed_revision
  AND workflow_github_effect_cleanups.observed_state = EXCLUDED.observed_state
  AND workflow_github_effect_cleanups.status = 'PENDING'`, job.ID, job.WorkflowID,
		observed.IssueNumber, nullableEpoch(pullRequestNumber), int64(observed.Revision), observed.State,
		int64(currentRevision), currentState)
	if err != nil {
		return fmt.Errorf("record superseded Workflow GitHub effect cleanup: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrWorkflowGitHubEffectFenceLost
	}
	return nil
}

func workflowGitHubEffectKind(kind string) (string, bool) {
	switch kind {
	case ReconcileGitHubLabelsJobKind, PublishHumanHandoffJobKind:
		return kind, true
	default:
		return "", false
	}
}

func validateWorkflowGitHubEffectContext(effect WorkflowGitHubEffectContext) error {
	if effect.WorkflowID == "" || effect.JobRevision == 0 || effect.RepositoryID <= 0 ||
		strings.TrimSpace(effect.RepositoryOwner) == "" || strings.TrimSpace(effect.RepositoryName) == "" ||
		effect.IssueNumber <= 0 || effect.State == "" || effect.Revision == 0 {
		return ErrWorkflowGitHubEffectFenceLost
	}
	if effect.ChangeProposal != nil && (effect.ChangeProposal.ID <= 0 || effect.ChangeProposal.Number <= 0 || strings.TrimSpace(effect.ChangeProposal.HeadSHA) == "") {
		return ErrWorkflowGitHubEffectFenceLost
	}
	return nil
}
