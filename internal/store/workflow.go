package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	// ErrWorkflowLocatorMismatch means normalized identity disagrees with the durable webhook envelope.
	ErrWorkflowLocatorMismatch = errors.New("workflow locator does not match webhook delivery")
	// ErrWorkflowDecisionInvalid means a transition returned an internally inconsistent decision.
	ErrWorkflowDecisionInvalid = errors.New("workflow transition returned an invalid decision")
	// ErrPendingEventReconciliationFenceLost means reconciliation ownership or immutable identity is stale.
	ErrPendingEventReconciliationFenceLost = errors.New("pending event reconciliation fence lost")
	// ErrPendingEventReconciliationPayloadInvalid means a fenced reconciliation job has an invalid durable payload.
	ErrPendingEventReconciliationPayloadInvalid = errors.New("pending event reconciliation payload is invalid")
	// ErrPendingNormalizedEventInvalid means persisted normalized event content is malformed or contradicts its durable envelope.
	ErrPendingNormalizedEventInvalid = errors.New("invalid pending normalized event")
	// ErrPendingEventCausalGap means no linked synchronization can advance the durable Change Proposal head.
	ErrPendingEventCausalGap = errors.New("pending event synchronization has a causal gap")
	// ErrClosureSettlementFenceLost means closure job ownership or immutable identity is stale.
	ErrClosureSettlementFenceLost = errors.New("closure settlement fence lost")
	// ErrClosureSettlementUnsettled means a closure stop or admitted mutation still needs acknowledgement.
	ErrClosureSettlementUnsettled = errors.New("closure settlement is unsettled")
	// ErrWorkflowSuccessorConflict means reconciliation found another live successor path.
	ErrWorkflowSuccessorConflict = errors.New("workflow already has a live successor")
	// ErrWorkflowNotFound means the requested Workflow does not exist.
	ErrWorkflowNotFound = errors.New("workflow not found")
)

const (
	WorkflowActionQueue           = "workflow"
	ReconcilePendingEventsJobKind = "RECONCILE_PENDING_EVENTS"
	StopAgentTurnJobKind          = "STOP_AGENT_TURN"
	SettleClosureJobKind          = "SETTLE_CLOSURE"
	PrepareAgentTurnJobKind       = "PREPARE_AGENT_TURN"
	stopAgentTurnJobPriority      = 100
	settleClosureJobPriority      = 80
)

// WorkflowLocator identifies a Workflow by its Work Item or known Change Proposal relation.
type WorkflowLocator struct {
	RepositoryID  int64
	IssueID       int64
	IssueNumber   int64
	PullRequestID int64
	WorkflowID    string
}

// WorkflowRepository is the immutable repository identity captured when a Workflow is created.
type WorkflowRepository struct {
	Owner string
	Name  string
}

// GetWorkflowRepository returns the immutable repository owner and name for a Workflow.
func (store *Store) GetWorkflowRepository(ctx context.Context, workflowID string) (WorkflowRepository, error) {
	if !validUUID(workflowID) {
		return WorkflowRepository{}, ErrWorkflowNotFound
	}
	var repository WorkflowRepository
	err := store.pool.QueryRow(ctx, `SELECT repository_owner, repository_name FROM workflows WHERE id = $1`, workflowID).Scan(&repository.Owner, &repository.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRepository{}, ErrWorkflowNotFound
	}
	if err != nil {
		return WorkflowRepository{}, fmt.Errorf("get Workflow repository: %w", err)
	}
	return repository, nil
}

// GetChangeProposalReview returns a previously recorded review identity in one repository.
func (store *Store) GetChangeProposalReview(ctx context.Context, repositoryID, reviewID int64) (*workflow.ReviewIdentity, error) {
	if repositoryID <= 0 || reviewID <= 0 {
		return nil, errors.New("get Change Proposal review: invalid identity")
	}
	var review workflow.ReviewIdentity
	err := store.pool.QueryRow(ctx, `
SELECT review.review_id, review.review_node_id, proposal.pull_request_id,
       review.actor_id, review.head_sha
FROM change_proposal_reviews AS review
JOIN change_proposals AS proposal ON proposal.id = review.change_proposal_id
WHERE review.repository_id = $1 AND review.review_id = $2`, repositoryID, reviewID).Scan(
		&review.ID, &review.NodeID, &review.ChangeProposalID, &review.ActorID, &review.HeadSHA,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Change Proposal review: %w", err)
	}
	return &review, nil
}

// WorkflowTransition applies a pure reducer transition to a relationally rehydrated Snapshot.
type WorkflowTransition func(workflow.Snapshot) workflow.Decision

// WorkflowApplication is the durable outcome of applying one normalized event.
type WorkflowApplication struct {
	DeliveryID  string
	WorkflowID  string
	Status      NormalizedEventStatus
	Disposition workflow.Disposition
	Reason      workflow.Reason
	State       workflow.State
	Revision    uint64
}

// PendingTransitionFactory rebuilds a pure transition from one persisted normalized event.
type PendingTransitionFactory func(NormalizedEventRecord) (WorkflowLocator, WorkflowTransition, error)

type workflowEnvelope struct {
	eventName, action               string
	repositoryID                    int64
	repositoryOwner, repositoryName string
	issueID, issueNumber            int64
}

// CompleteWebhookTransition atomically applies a claimed webhook's normalized Workflow transition.
func (store *Store) CompleteWebhookTransition(ctx context.Context, deliveryID, claimToken string, normalizedPayload json.RawMessage, locator WorkflowLocator, transition WorkflowTransition) (WorkflowApplication, error) {
	if !validUUID(deliveryID) || !validUUID(claimToken) {
		return WorkflowApplication{}, ErrWebhookClaimLost
	}
	payload, err := canonicalJSON(normalizedPayload)
	if err != nil {
		return WorkflowApplication{}, fmt.Errorf("complete webhook transition: normalized payload: %w", err)
	}
	if transition == nil {
		return WorkflowApplication{}, errors.New("complete webhook transition: transition is nil")
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowApplication{}, fmt.Errorf("begin webhook transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := validateNormalizedDeliveryID(payload, deliveryID); err != nil {
		return WorkflowApplication{}, fmt.Errorf("complete webhook transition: %w", err)
	}

	envelope, err := lockWebhookForTransition(ctx, tx, deliveryID, claimToken)
	if err != nil {
		return WorkflowApplication{}, err
	}
	if err := validateWorkflowLocator(locator, envelope); err != nil {
		return WorkflowApplication{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO normalized_events (delivery_id, payload, status) VALUES ($1, $2, 'PENDING')`, deliveryID, payload); err != nil {
		return WorkflowApplication{}, fmt.Errorf("insert normalized event: %w", err)
	}
	record := NormalizedEventRecord{DeliveryID: deliveryID, Payload: payload, Status: NormalizedEventPending}
	application, err := applyWorkflowTransitionTx(ctx, tx, record, envelope, locator, transition)
	if err != nil {
		return WorkflowApplication{}, err
	}
	result, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET workflow_id = $3, status = 'PROCESSED', claim_owner = NULL, claim_token = NULL,
    claimed_at = NULL, lease_expires_at = NULL, processed_at = clock_timestamp(), last_error = NULL
WHERE delivery_id = $1 AND status = 'PROCESSING' AND claim_token = $2
  AND lease_expires_at > clock_timestamp()`, deliveryID, claimToken, nullableString(application.WorkflowID))
	if err != nil {
		return WorkflowApplication{}, fmt.Errorf("complete transitioned webhook delivery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return WorkflowApplication{}, ErrWebhookClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowApplication{}, fmt.Errorf("commit webhook transition: %w", err)
	}
	return application, nil
}

// ApplyNextPendingNormalizedEvent drains one historical Phase 4 event without polling DEFERRED rows.
// Its inbox delivery was already completed, so this API cannot restore claim fencing for historical events.
func (store *Store) ApplyNextPendingNormalizedEvent(ctx context.Context, factory PendingTransitionFactory) (WorkflowApplication, bool, error) {
	if factory == nil {
		return WorkflowApplication{}, false, errors.New("apply pending normalized event: factory is nil")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowApplication{}, false, fmt.Errorf("begin pending normalized event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var record NormalizedEventRecord
	var envelope workflowEnvelope
	err = tx.QueryRow(ctx, `
SELECT event.delivery_id::text, event.payload, event.status, event.created_at,
       COALESCE(delivery.repository_id, 0), COALESCE(delivery.repository_owner, ''),
       COALESCE(delivery.repository_name, ''), COALESCE(delivery.issue_id, 0),
       COALESCE(delivery.issue_number, 0)
FROM normalized_events AS event
JOIN webhook_deliveries AS delivery USING (delivery_id)
WHERE event.status = 'PENDING'
ORDER BY event.created_at, event.delivery_id
FOR UPDATE OF event SKIP LOCKED
LIMIT 1`).Scan(&record.DeliveryID, &record.Payload, &record.Status, &record.CreatedAt,
		&envelope.repositoryID, &envelope.repositoryOwner, &envelope.repositoryName,
		&envelope.issueID, &envelope.issueNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return WorkflowApplication{}, false, fmt.Errorf("commit empty pending event drain: %w", err)
		}
		return WorkflowApplication{}, false, nil
	}
	if err != nil {
		return WorkflowApplication{}, false, fmt.Errorf("select pending normalized event: %w", err)
	}
	if err := validateNormalizedDeliveryID(record.Payload, record.DeliveryID); err != nil {
		return WorkflowApplication{}, false, fmt.Errorf("apply pending normalized event: %w", err)
	}
	locator, transition, err := factory(record)
	if err != nil {
		return WorkflowApplication{}, false, fmt.Errorf("build pending transition: %w", err)
	}
	if transition == nil {
		return WorkflowApplication{}, false, errors.New("build pending transition: transition is nil")
	}
	if err := validateWorkflowLocator(locator, envelope); err != nil {
		return WorkflowApplication{}, false, err
	}
	application, err := applyWorkflowTransitionTx(ctx, tx, record, envelope, locator, transition)
	if err != nil {
		return WorkflowApplication{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowApplication{}, false, fmt.Errorf("commit pending normalized event: %w", err)
	}
	return application, true, nil
}

func lockWebhookForTransition(ctx context.Context, tx pgx.Tx, deliveryID, claimToken string) (workflowEnvelope, error) {
	var envelope workflowEnvelope
	var status WebhookStatus
	var currentToken *string
	var leaseLive bool
	err := tx.QueryRow(ctx, `
SELECT status, claim_token::text, COALESCE(lease_expires_at > clock_timestamp(), FALSE),
       COALESCE(repository_id, 0), COALESCE(repository_owner, ''), COALESCE(repository_name, ''),
       COALESCE(issue_id, 0), COALESCE(issue_number, 0)
FROM webhook_deliveries WHERE delivery_id = $1 FOR UPDATE`, deliveryID).Scan(
		&status, &currentToken, &leaseLive, &envelope.repositoryID, &envelope.repositoryOwner,
		&envelope.repositoryName, &envelope.issueID, &envelope.issueNumber,
	)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (status != WebhookProcessing || currentToken == nil || *currentToken != claimToken || !leaseLive) {
		return workflowEnvelope{}, ErrWebhookClaimLost
	}
	if err != nil {
		return workflowEnvelope{}, fmt.Errorf("lock webhook transition: %w", err)
	}
	return envelope, nil
}

func validateWorkflowLocator(locator WorkflowLocator, envelope workflowEnvelope) error {
	if locator.RepositoryID <= 0 || locator.RepositoryID != envelope.repositoryID ||
		(locator.IssueID == 0) != (locator.IssueNumber == 0) || locator.IssueID < 0 || locator.IssueNumber < 0 || locator.PullRequestID < 0 ||
		(locator.WorkflowID != "" && !validUUID(locator.WorkflowID)) {
		return ErrWorkflowLocatorMismatch
	}
	if envelope.issueID > 0 && (locator.IssueID != envelope.issueID || locator.IssueNumber != envelope.issueNumber) {
		return ErrWorkflowLocatorMismatch
	}
	if locator.IssueID == 0 && locator.PullRequestID == 0 {
		return ErrWorkflowLocatorMismatch
	}
	return nil
}

func applyWorkflowTransitionTx(ctx context.Context, tx pgx.Tx, event NormalizedEventRecord, envelope workflowEnvelope, locator WorkflowLocator, transition WorkflowTransition) (WorkflowApplication, error) {
	workflowID, err := resolveWorkflowID(ctx, tx, locator)
	if err != nil {
		return WorkflowApplication{}, err
	}
	if workflowID == "" && locator.IssueID > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fmt.Sprintf("%d:%d", locator.RepositoryID, locator.IssueID)); err != nil {
			return WorkflowApplication{}, fmt.Errorf("serialize first workflow trigger: %w", err)
		}
		workflowID, err = resolveWorkflowID(ctx, tx, locator)
		if err != nil {
			return WorkflowApplication{}, err
		}
	}

	snapshot := workflow.Snapshot{State: workflow.StateAbsent}
	if workflowID != "" {
		snapshot, err = rehydrateWorkflow(ctx, tx, workflowID)
		if err != nil {
			return WorkflowApplication{}, err
		}
	}
	decision := transition(snapshot)
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return WorkflowApplication{}, err
	}

	if decision.Disposition == workflow.DispositionApplied {
		if workflowID == "" {
			if locator.IssueID == 0 || decision.Snapshot.WorkItem != (workflow.WorkItem{RepositoryID: locator.RepositoryID, IssueID: locator.IssueID, IssueNumber: locator.IssueNumber}) {
				return WorkflowApplication{}, ErrWorkflowDecisionInvalid
			}
			workflowID, err = randomUUID()
			if err != nil {
				return WorkflowApplication{}, fmt.Errorf("generate workflow identity: %w", err)
			}
			if err := insertWorkflow(ctx, tx, workflowID, envelope, decision.Snapshot); err != nil {
				return WorkflowApplication{}, err
			}
		}
		if err := persistAppliedDecision(ctx, tx, event.DeliveryID, workflowID, snapshot, decision, ""); err != nil {
			return WorkflowApplication{}, err
		}
	}

	status := NormalizedEventCompleted
	deferredTurnID := ""
	if decision.Disposition == workflow.DispositionDeferred {
		status = NormalizedEventDeferred
		if snapshot.ActiveTurn != nil {
			deferredTurnID = snapshot.ActiveTurn.ID
		}
	}
	result, err := tx.Exec(ctx, `
UPDATE normalized_events
SET status = $2, workflow_id = $3, disposition = $4, reason = $5,
    applied_revision = $6, deferred_for_turn_id = $7, processed_at = clock_timestamp()
WHERE delivery_id = $1 AND status = 'PENDING'`, event.DeliveryID, status,
		nullableString(workflowID), decision.Disposition, decision.Reason,
		int64(decision.Snapshot.Revision), nullableString(deferredTurnID))
	if err != nil {
		return WorkflowApplication{}, fmt.Errorf("complete normalized event: %w", err)
	}
	if result.RowsAffected() != 1 {
		return WorkflowApplication{}, errors.New("normalized event is no longer pending")
	}
	return WorkflowApplication{
		DeliveryID: event.DeliveryID, WorkflowID: workflowID, Status: status,
		Disposition: decision.Disposition, Reason: decision.Reason,
		State: decision.Snapshot.State, Revision: decision.Snapshot.Revision,
	}, nil
}

func resolveWorkflowID(ctx context.Context, tx pgx.Tx, locator WorkflowLocator) (string, error) {
	var byIssue, byProposal, byMarker string
	if locator.IssueID > 0 {
		err := tx.QueryRow(ctx, `
SELECT id::text FROM workflows
WHERE repository_id = $1 AND issue_id = $2 AND issue_number = $3`,
			locator.RepositoryID, locator.IssueID, locator.IssueNumber).Scan(&byIssue)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("resolve workflow by issue: %w", err)
		}
	}
	if locator.PullRequestID > 0 {
		err := tx.QueryRow(ctx, `
SELECT workflow_id::text FROM change_proposals
WHERE repository_id = $1 AND pull_request_id = $2`, locator.RepositoryID, locator.PullRequestID).Scan(&byProposal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("resolve workflow by change proposal: %w", err)
		}
	}
	if locator.WorkflowID != "" {
		err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 AND repository_id = $2`, locator.WorkflowID, locator.RepositoryID).Scan(&byMarker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("resolve workflow by marker: %w", err)
		}
		if byMarker == "" && (byIssue != "" || byProposal != "") {
			return "", ErrWorkflowLocatorMismatch
		}
	}
	resolved := ""
	for _, candidate := range []string{byIssue, byProposal, byMarker} {
		if candidate == "" {
			continue
		}
		if resolved != "" && resolved != candidate {
			return "", ErrWorkflowLocatorMismatch
		}
		resolved = candidate
	}
	if byIssue != "" && byProposal != "" && byIssue != byProposal {
		return "", ErrWorkflowLocatorMismatch
	}
	return resolved, nil
}

func rehydrateWorkflow(ctx context.Context, tx pgx.Tx, workflowID string) (workflow.Snapshot, error) {
	var snapshot workflow.Snapshot
	var resumeRole, assignmentStatus, runtimeState, closureID, closureToken, retentionToken string
	var closureDeadline, retentionDeadline *time.Time
	var closureReopen bool
	err := tx.QueryRow(ctx, `
SELECT status, state_revision, repository_id, issue_id, issue_number,
       COALESCE(resume_role, ''), COALESCE(desired_assignment_status, ''),
       COALESCE(desired_runtime_state, ''), COALESCE(closure_id, ''), closure_deadline,
       COALESCE(closure_retention_token, ''), closure_reopen_requested,
       retention_deadline, COALESCE(retention_token, '')
FROM workflows WHERE id = $1 FOR UPDATE`, workflowID).Scan(
		&snapshot.State, &snapshot.Revision, &snapshot.WorkItem.RepositoryID,
		&snapshot.WorkItem.IssueID, &snapshot.WorkItem.IssueNumber, &resumeRole,
		&assignmentStatus, &runtimeState, &closureID, &closureDeadline, &closureToken,
		&closureReopen, &retentionDeadline, &retentionToken,
	)
	if err != nil {
		return workflow.Snapshot{}, fmt.Errorf("lock workflow: %w", err)
	}
	snapshot.ResumeRole = workflow.Role(resumeRole)
	snapshot.Assignments.Status = workflow.AssignmentStatus(assignmentStatus)
	snapshot.Assignments.RuntimeState = workflow.RuntimeState(runtimeState)
	if retentionDeadline != nil {
		snapshot.Assignments.RetainedUntil = *retentionDeadline
		snapshot.Assignments.RetentionToken = retentionToken
	}
	if closureID != "" {
		snapshot.Closure = &workflow.Closure{ID: closureID, RetainUntil: *closureDeadline, RetentionToken: closureToken, ReopenRequested: closureReopen}
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(attempt_number), 0) FROM workflow_attempts WHERE workflow_id = $1`, workflowID).Scan(&snapshot.LastAttemptNumber); err != nil {
		return workflow.Snapshot{}, fmt.Errorf("rehydrate attempt sequence: %w", err)
	}
	var attempt workflow.WorkflowAttempt
	err = tx.QueryRow(ctx, `
SELECT id::text, attempt_number, started_at, review_cycles_completed, review_cycle_limit,
       infrastructure_failures, infrastructure_failure_limit
FROM workflow_attempts WHERE workflow_id = $1 AND active`, workflowID).Scan(
		&attempt.ID, &attempt.Number, &attempt.StartedAt, &attempt.ReviewBudget.Used,
		&attempt.ReviewBudget.Limit, &attempt.InfrastructureRetryBudget.Used,
		&attempt.InfrastructureRetryBudget.Limit)
	if err == nil {
		attempt.Lifecycle = workflow.AttemptActive
		snapshot.CurrentAttempt = &attempt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return workflow.Snapshot{}, fmt.Errorf("rehydrate active attempt: %w", err)
	}
	var proposal workflow.ChangeProposal
	err = tx.QueryRow(ctx, `
SELECT pull_request_id, pull_request_number, head_sha, active, COALESCE(ready_for_sha, '')
FROM change_proposals WHERE workflow_id = $1 AND active`, workflowID).Scan(
		&proposal.ID, &proposal.Number, &proposal.HeadSHA, &proposal.Open, &proposal.ReadyForSHA)
	if err == nil {
		snapshot.ChangeProposal = &proposal
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return workflow.Snapshot{}, fmt.Errorf("rehydrate active change proposal: %w", err)
	}
	var turn workflow.ActiveTurn
	err = tx.QueryRow(ctx, `
SELECT turn.id::text, turn.agent_session_id::text, turn.workflow_attempt_id::text,
       assignment.role, turn.execution_epoch, turn.control_revision,
       COALESCE(proposal.pull_request_id, 0), COALESCE(turn.expected_head_sha, '')
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
LEFT JOIN change_proposals AS proposal ON proposal.id = turn.change_proposal_id
WHERE assignment.workflow_id = $1 AND turn.active`, workflowID).Scan(
		&turn.ID, &turn.SessionID, &turn.AttemptID, &turn.Role, &turn.Epoch,
		&turn.ControlRevision, &turn.ChangeProposalID, &turn.ExpectedHeadSHA)
	if err == nil {
		snapshot.ActiveTurn = &turn
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return workflow.Snapshot{}, fmt.Errorf("rehydrate active turn: %w", err)
	}
	return snapshot, nil
}

func validateWorkflowDecision(current workflow.Snapshot, decision workflow.Decision) error {
	switch decision.Disposition {
	case workflow.DispositionApplied:
		if decision.Snapshot.Revision != current.Revision+1 || decision.Snapshot.State == workflow.StateAbsent || decision.Reason == "" ||
			current.State != workflow.StateAbsent && decision.Snapshot.WorkItem != current.WorkItem {
			return ErrWorkflowDecisionInvalid
		}
	case workflow.DispositionDeferred, workflow.DispositionDuplicate, workflow.DispositionStale, workflow.DispositionUnrelated, workflow.DispositionIllegal:
		if !reflect.DeepEqual(decision.Snapshot, current) || decision.Reason == "" {
			return ErrWorkflowDecisionInvalid
		}
		if decision.Disposition != workflow.DispositionDeferred && len(decision.Actions) != 0 {
			return ErrWorkflowDecisionInvalid
		}
	default:
		return ErrWorkflowDecisionInvalid
	}
	return nil
}

func insertWorkflow(ctx context.Context, tx pgx.Tx, workflowID string, envelope workflowEnvelope, snapshot workflow.Snapshot) error {
	_, err := tx.Exec(ctx, `
INSERT INTO workflows (
    id, repository_id, repository_owner, repository_name, issue_id, issue_number,
    status, state_revision, resume_role, desired_assignment_status, desired_runtime_state
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, workflowID,
		snapshot.WorkItem.RepositoryID, envelope.repositoryOwner, envelope.repositoryName,
		snapshot.WorkItem.IssueID, snapshot.WorkItem.IssueNumber, snapshot.State,
		int64(snapshot.Revision), nullableString(string(snapshot.ResumeRole)),
		nullableString(string(snapshot.Assignments.Status)), nullableString(string(snapshot.Assignments.RuntimeState)))
	if err != nil {
		return fmt.Errorf("insert workflow: %w", err)
	}
	return nil
}

func persistAppliedDecision(ctx context.Context, tx pgx.Tx, deliveryID, workflowID string, current workflow.Snapshot, decision workflow.Decision, actionNamespace string) error {
	return persistAppliedDecisionWithProvenance(ctx, tx, deliveryID, "", workflowID, current, decision, actionNamespace)
}

func persistAppliedSettlementDecision(ctx context.Context, tx pgx.Tx, settlementID, workflowID string, current workflow.Snapshot, decision workflow.Decision) error {
	return persistAppliedDecisionWithProvenance(ctx, tx, "", settlementID, workflowID, current, decision, "")
}

func persistAppliedDecisionWithProvenance(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, workflowID string, current workflow.Snapshot, decision workflow.Decision, actionNamespace string) error {
	if decision.Reason == workflow.ReasonClosureSettled {
		if err := requireClosureSettlementBarrier(ctx, tx, workflowID, current); err != nil {
			return err
		}
	}
	for _, action := range decision.Actions {
		if complete, ok := action.(workflow.CompleteAttemptAction); ok {
			result, err := tx.Exec(ctx, `
UPDATE workflow_attempts SET active = FALSE, status = $3, completed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1 AND workflow_id = $2 AND active`, complete.AttemptID, workflowID, complete.Reason)
			if err != nil || result.RowsAffected() != 1 {
				if err == nil {
					err = errors.New("active attempt was not completed")
				}
				return fmt.Errorf("complete workflow attempt: %w", err)
			}
		}
	}
	if err := persistWorkflowSnapshot(ctx, tx, workflowID, decision); err != nil {
		return err
	}
	for _, action := range decision.Actions {
		if create, ok := action.(workflow.CreateAttemptAction); ok {
			attempt := create.Attempt
			if _, err := tx.Exec(ctx, `
INSERT INTO workflow_attempts (
    id, workflow_id, attempt_number, trigger_delivery_id, status, active,
    review_cycles_completed, review_cycle_limit, infrastructure_failures,
    infrastructure_failure_limit, started_at
)
VALUES ($1, $2, $3, $4, 'ACTIVE', TRUE, $5, $6, $7, $8, $9)`, attempt.ID,
				workflowID, int64(attempt.Number), deliveryID, int(attempt.ReviewBudget.Used),
				int(attempt.ReviewBudget.Limit), int(attempt.InfrastructureRetryBudget.Used),
				int(attempt.InfrastructureRetryBudget.Limit), attempt.StartedAt); err != nil {
				return fmt.Errorf("create workflow attempt: %w", err)
			}
		}
	}
	if decision.Snapshot.CurrentAttempt != nil {
		attempt := decision.Snapshot.CurrentAttempt
		if _, err := tx.Exec(ctx, `
UPDATE workflow_attempts SET review_cycles_completed = $3, review_cycle_limit = $4,
    infrastructure_failures = $5, infrastructure_failure_limit = $6, updated_at = clock_timestamp()
WHERE id = $1 AND workflow_id = $2 AND active`, attempt.ID, workflowID,
			int(attempt.ReviewBudget.Used), int(attempt.ReviewBudget.Limit),
			int(attempt.InfrastructureRetryBudget.Used), int(attempt.InfrastructureRetryBudget.Limit)); err != nil {
			return fmt.Errorf("persist workflow attempt budgets: %w", err)
		}
	}
	if err := persistWorkflowActionsWithProvenance(ctx, tx, deliveryID, settlementID, workflowID, decision, actionNamespace); err != nil {
		return err
	}
	return nil
}

func requireClosureSettlementBarrier(ctx context.Context, tx pgx.Tx, workflowID string, current workflow.Snapshot) error {
	if current.Closure == nil {
		return ErrClosureSettlementFenceLost
	}
	var barrierRevision int64
	var settlementStatus JobStatus
	var settled bool
	err := tx.QueryRow(ctx, `
SELECT barrier.workflow_revision, barrier.settled_at IS NOT NULL, job.status
FROM workflow_closure_barriers AS barrier
JOIN jobs AS job ON job.id = barrier.settlement_job_id
WHERE barrier.workflow_id = $1 AND barrier.closure_id = $2
FOR UPDATE OF barrier, job`, workflowID, current.Closure.ID).Scan(&barrierRevision, &settled, &settlementStatus)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!settled || settlementStatus != JobSucceeded || barrierRevision > int64(current.Revision)) {
		return ErrClosureSettlementUnsettled
	}
	if err != nil {
		return fmt.Errorf("verify closure settlement barrier: %w", err)
	}
	return nil
}

func persistWorkflowSnapshot(ctx context.Context, tx pgx.Tx, workflowID string, decision workflow.Decision) error {
	snapshot := decision.Snapshot
	var closureID, closureToken string
	var closureDeadline any
	closureReopen := false
	if snapshot.Closure != nil {
		closureID, closureToken = snapshot.Closure.ID, snapshot.Closure.RetentionToken
		closureDeadline, closureReopen = snapshot.Closure.RetainUntil, snapshot.Closure.ReopenRequested
	}
	var retentionDeadline any
	if !snapshot.Assignments.RetainedUntil.IsZero() {
		retentionDeadline = snapshot.Assignments.RetainedUntil
	}
	handoffReason := any(nil)
	for _, action := range decision.Actions {
		if handoff, ok := action.(workflow.MarkHumanHandoffAction); ok {
			handoffReason = string(handoff.Reason)
		}
	}
	result, err := tx.Exec(ctx, `
UPDATE workflows SET status = $2, state_revision = $3, resume_role = $4,
    desired_assignment_status = $5, desired_runtime_state = $6,
    closure_id = $7, closure_deadline = $8, closure_retention_token = $9,
    closure_reopen_requested = $10, retention_deadline = $11, retention_token = $12,
    human_handoff_reason = $13, updated_at = clock_timestamp(),
    closed_at = CASE WHEN $2 = 'CLOSED' THEN COALESCE(closed_at, clock_timestamp()) ELSE NULL END
WHERE id = $1`, workflowID, snapshot.State, int64(snapshot.Revision),
		nullableString(string(snapshot.ResumeRole)), nullableString(string(snapshot.Assignments.Status)),
		nullableString(string(snapshot.Assignments.RuntimeState)), nullableString(closureID), closureDeadline,
		nullableString(closureToken), closureReopen, retentionDeadline,
		nullableString(snapshot.Assignments.RetentionToken), handoffReason)
	if err != nil {
		return fmt.Errorf("persist workflow snapshot: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("persist workflow snapshot: workflow not found")
	}
	if err := persistChangeProposal(ctx, tx, workflowID, snapshot); err != nil {
		return err
	}
	return nil
}

func persistChangeProposal(ctx context.Context, tx pgx.Tx, workflowID string, snapshot workflow.Snapshot) error {
	if snapshot.ChangeProposal == nil {
		return nil
	}
	proposal := snapshot.ChangeProposal
	result, err := tx.Exec(ctx, `
UPDATE change_proposals
SET head_sha = $4, ready_for_sha = $5, updated_at = clock_timestamp()
WHERE workflow_id = $1
  AND repository_id = (SELECT repository_id FROM workflows WHERE id = $1)
  AND pull_request_id = $2 AND pull_request_number = $3 AND active`,
		workflowID, proposal.ID, proposal.Number, proposal.HeadSHA, nullableString(proposal.ReadyForSHA))
	if err != nil {
		return fmt.Errorf("persist change proposal: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("persist change proposal: matching active Change Proposal not found")
	}
	return nil
}

type preparedTurnIntent struct {
	mode workflow.AssignmentGeneration
	turn workflow.EnqueueTurnAction
	set  bool
}

func persistWorkflowActions(ctx context.Context, tx pgx.Tx, deliveryID, workflowID string, decision workflow.Decision, actionNamespace string) error {
	return persistWorkflowActionsWithProvenance(ctx, tx, deliveryID, "", workflowID, decision, actionNamespace)
}

func persistWorkflowActionsWithProvenance(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, workflowID string, decision workflow.Decision, actionNamespace string) error {
	intent := preparedTurnIntent{mode: workflow.AssignmentGenerationCurrent}
	labels := false
	consumeRun := false
	var closureTurn *workflow.TurnGuard
	var closureStopJobID string
	var labelAction workflow.ReconcileLabelsAction
	for _, action := range decision.Actions {
		switch action := action.(type) {
		case workflow.EnsureAssignmentsAction:
			intent.mode = action.Mode
		case workflow.EnqueueTurnAction:
			intent.turn, intent.set = action, true
		case workflow.ConsumeRunLabelAction:
			labels, consumeRun = true, true
		case workflow.ReconcileLabelsAction:
			labels, labelAction = true, action
		case workflow.RecordReviewAction:
			if err := recordChangeProposalReview(ctx, tx, deliveryID, settlementID, workflowID, action); err != nil {
				return err
			}
		case workflow.MarkHumanHandoffAction:
			if decision.Snapshot.CurrentAttempt != nil {
				if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET human_handoff_reason = $2 WHERE id = $1`, decision.Snapshot.CurrentAttempt.ID, action.Reason); err != nil {
					return fmt.Errorf("mark attempt human handoff: %w", err)
				}
			}
			handoffPayload := map[string]any{
				"reason": action.Reason, "diagnostic": action.Diagnostic, "revision": decision.Snapshot.Revision,
			}
			if decision.Snapshot.ChangeProposal != nil {
				handoffPayload["pull_request_number"] = decision.Snapshot.ChangeProposal.Number
			}
			if err := enqueueWorkflowJobWithProvenance(ctx, tx, deliveryID, settlementID, workflowID, currentAttemptID(decision.Snapshot), namespacedWorkflowActionKey(actionNamespace, "publish-human-handoff"), "PUBLISH_HUMAN_HANDOFF", handoffPayload, nil); err != nil {
				return err
			}
		case workflow.CloseMutationAdmissionAction:
			if err := closeMutationAdmissionTx(ctx, tx, action.Turn); err != nil {
				return err
			}
			turn := action.Turn
			closureTurn = &turn
		case workflow.StopTurnAction:
			if decision.Snapshot.Closure == nil {
				return ErrWorkflowDecisionInvalid
			}
			var err error
			closureStopJobID, err = enqueueStopTurnJob(ctx, tx, deliveryID, workflowID, decision.Snapshot.Closure.ID, action.Turn, decision.Snapshot.Revision)
			if err != nil {
				return err
			}
		case workflow.SettleClosureAction:
			settlementJobID, err := enqueueClosureSettlementJob(ctx, tx, deliveryID, workflowID, currentAttemptID(decision.Snapshot), action.ClosureID, decision.Snapshot.Revision, closureTurn)
			if err != nil {
				return err
			}
			if err := createClosureBarrier(ctx, tx, workflowID, action.ClosureID, decision.Snapshot.Revision, closureTurn, closureStopJobID, settlementJobID); err != nil {
				return err
			}
		case workflow.CompleteAssignmentsAction:
			if err := enqueueWorkflowJob(ctx, tx, deliveryID, workflowID, "", "complete-assignments", "COMPLETE_ASSIGNMENTS", map[string]any{"revision": decision.Snapshot.Revision}, nil); err != nil {
				return err
			}
		case workflow.ScheduleRetentionAction:
			deadline := action.RetainUntil
			if err := enqueueWorkflowJob(ctx, tx, deliveryID, workflowID, "", "collect-retention", "COLLECT_ASSIGNMENTS", map[string]any{"retention_token": action.RetentionToken, "retain_until": action.RetainUntil, "revision": decision.Snapshot.Revision}, &deadline); err != nil {
				return err
			}
		case workflow.CancelRetentionAction:
			if _, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'CANCELLED', completed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE workflow_id = $1 AND kind = 'COLLECT_ASSIGNMENTS' AND status = 'AVAILABLE'
  AND payload->>'retention_token' = $2`, workflowID, action.RetentionToken); err != nil {
				return fmt.Errorf("cancel assignment retention: %w", err)
			}
			if err := enqueueWorkflowJob(ctx, tx, deliveryID, workflowID, "", "cancel-retention", "CANCEL_ASSIGNMENT_RETENTION", map[string]any{"retention_token": action.RetentionToken, "revision": decision.Snapshot.Revision}, nil); err != nil {
				return err
			}
		case workflow.ReconcilePendingEventsAction:
			if err := enqueueReconcilePendingEventsJob(ctx, tx, deliveryID, settlementID, workflowID, currentAttemptID(decision.Snapshot), decision.Snapshot.Revision, action); err != nil {
				return err
			}
		case workflow.RecordPendingEventAction, workflow.CompleteAttemptAction, workflow.CreateAttemptAction:
		default:
			return fmt.Errorf("persist workflow action %T: %w", action, ErrWorkflowDecisionInvalid)
		}
	}
	if intent.set {
		if err := enqueueWorkflowJobWithProvenance(ctx, tx, deliveryID, settlementID, workflowID, currentAttemptID(decision.Snapshot), "prepare-agent-turn", PrepareAgentTurnJobKind, map[string]any{
			"mode": intent.mode, "role": intent.turn.Role, "purpose": intent.turn.Purpose,
			"expected_head_sha": intent.turn.ExpectedHeadSHA, "retry_of_turn_id": intent.turn.RetryOfTurnID,
			"revision": decision.Snapshot.Revision,
		}, nil); err != nil {
			return err
		}
	}
	if labels {
		if labelAction.State == "" {
			labelAction.State = decision.Snapshot.State
		}
		if err := enqueueWorkflowJobWithProvenance(ctx, tx, deliveryID, settlementID, workflowID, currentAttemptID(decision.Snapshot), namespacedWorkflowActionKey(actionNamespace, "reconcile-github-labels"), "RECONCILE_GITHUB_LABELS", map[string]any{
			"state": labelAction.State, "ready_for_sha": labelAction.ReadyForSHA,
			"consume_run": consumeRun, "revision": decision.Snapshot.Revision,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func namespacedWorkflowActionKey(namespace, action string) string {
	if namespace == "" {
		return action
	}
	return namespace + ":" + action
}

func enqueueReconcilePendingEventsJob(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, workflowID, attemptID string, revision uint64, action workflow.ReconcilePendingEventsAction) error {
	rows, err := tx.Query(ctx, `
SELECT event.delivery_id::text, event.deferred_for_turn_id::text,
	   turn.execution_epoch, turn.control_revision, turn.agent_session_id::text,
	   turn.workflow_attempt_id::text, session.agent_assignment_id::text,
	   assignment.role
FROM normalized_events AS event
JOIN agent_turns AS turn ON turn.id = event.deferred_for_turn_id
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE event.workflow_id = $1 AND event.status = 'DEFERRED'
  AND event.deferred_for_turn_id = $2
ORDER BY event.created_at, event.delivery_id`, workflowID, action.SourceTurn.TurnID)
	if err != nil {
		return fmt.Errorf("read deferred normalized events for reconciliation: %w", err)
	}
	deferredIDs := make([]string, 0, action.Count)
	var sourceTurnID, sourceSessionID, sourceAssignmentID string
	var sourceEpoch, sourceControlRevision int64
	for rows.Next() {
		var eventID, turnID, sessionID, turnAttemptID, assignmentID string
		var role workflow.Role
		var epoch, controlRevision int64
		if err := rows.Scan(&eventID, &turnID, &epoch, &controlRevision, &sessionID, &turnAttemptID, &assignmentID, &role); err != nil {
			rows.Close()
			return fmt.Errorf("scan deferred normalized event: %w", err)
		}
		if turnID != action.SourceTurn.TurnID || sessionID != action.SourceTurn.SessionID ||
			turnAttemptID != action.SourceTurn.AttemptID || turnAttemptID != attemptID || role != action.SourceTurn.Role ||
			epoch != int64(action.SourceTurn.Epoch) || controlRevision != int64(action.SourceTurn.ControlRevision) {
			rows.Close()
			return ErrAgentTurnFenceLost
		}
		if sourceTurnID == "" {
			sourceTurnID, sourceEpoch, sourceControlRevision = turnID, epoch, controlRevision
			sourceSessionID, sourceAssignmentID = sessionID, assignmentID
		} else if sourceTurnID != turnID || sourceSessionID != sessionID || sourceAssignmentID != assignmentID || sourceEpoch != epoch || sourceControlRevision != controlRevision {
			rows.Close()
			return errors.New("reconcile pending events: deferred rows span multiple Agent Turn fences")
		}
		deferredIDs = append(deferredIDs, eventID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read deferred normalized events: %w", err)
	}
	rows.Close()
	if sourceTurnID == "" || uint32(len(deferredIDs)) != action.Count {
		return errors.New("reconcile pending events: deferred row count does not match action")
	}
	payload := map[string]any{
		"workflow_id": workflowID, "workflow_attempt_id": attemptID,
		"count": action.Count, "latest_observed_head_sha": action.LatestObservedHeadSHA,
		"fallback_role": action.FallbackRole, "fallback_purpose": action.FallbackPurpose,
		"fallback_expected_head_sha": action.FallbackExpectedHead, "retry_of_turn_id": action.RetryOfTurnID,
		"revision": revision, "source_turn_id": sourceTurnID,
		"source_execution_epoch": sourceEpoch, "source_control_revision": sourceControlRevision,
		"deferred_normalized_event_ids": deferredIDs,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode pending-event reconciliation: %w", err)
	}
	jobID, err := randomUUID()
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, normalized_event_id,
    agent_turn_settlement_id, action_key
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, clock_timestamp(), 3,
        $5, $6, $7, $8, $9, $10, $11, $12, $13, 'reconcile-pending-events')
ON CONFLICT (normalized_event_id, action_key) WHERE normalized_event_id IS NOT NULL DO NOTHING`,
		jobID, WorkflowActionQueue, ReconcilePendingEventsJobKind, payloadJSON,
		workflowActionIdempotencyKey(workflowID, deliveryID, settlementID, "reconcile-pending-events"),
		workflowID, attemptID, sourceAssignmentID, sourceSessionID, sourceTurnID, sourceEpoch,
		nullableString(deliveryID), nullableString(settlementID))
	if err != nil {
		return fmt.Errorf("enqueue pending-event reconciliation: %w", err)
	}
	if result.RowsAffected() == 0 {
		var existingPayload []byte
		if err := tx.QueryRow(ctx, `
SELECT id::text, payload FROM jobs
WHERE (normalized_event_id = $1 OR agent_turn_settlement_id = $2)
  AND action_key = 'reconcile-pending-events'`, nullableString(deliveryID), nullableString(settlementID)).Scan(&jobID, &existingPayload); err != nil {
			return fmt.Errorf("read pending-event reconciliation job: %w", err)
		}
		canonicalExisting, _ := canonicalJSON(existingPayload)
		canonicalPayload, _ := canonicalJSON(payloadJSON)
		if !reflect.DeepEqual(canonicalExisting, canonicalPayload) {
			return ErrJobIdempotencyConflict
		}
	}
	for _, eventID := range deferredIDs {
		result, err := tx.Exec(ctx, `
INSERT INTO job_normalized_events (job_id, normalized_event_id)
VALUES ($1, $2) ON CONFLICT (normalized_event_id) DO NOTHING`, jobID, eventID)
		if err != nil {
			return fmt.Errorf("link deferred normalized event to reconciliation job: %w", err)
		}
		if result.RowsAffected() == 0 {
			var existingJobID string
			if err := tx.QueryRow(ctx, `SELECT job_id::text FROM job_normalized_events WHERE normalized_event_id = $1`, eventID).Scan(&existingJobID); err != nil {
				return fmt.Errorf("read deferred normalized event job link: %w", err)
			}
			if existingJobID != jobID {
				return ErrJobIdempotencyConflict
			}
		}
	}
	return nil
}

func currentAttemptID(snapshot workflow.Snapshot) string {
	if snapshot.CurrentAttempt == nil {
		return ""
	}
	return snapshot.CurrentAttempt.ID
}

func enqueueWorkflowJob(ctx context.Context, tx pgx.Tx, deliveryID, workflowID, attemptID, actionKey, kind string, value any, availableAt *time.Time) error {
	return enqueueWorkflowJobWithProvenance(ctx, tx, deliveryID, "", workflowID, attemptID, actionKey, kind, value, availableAt)
}

func enqueueWorkflowJobWithProvenance(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, workflowID, attemptID, actionKey, kind string, value any, availableAt *time.Time) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s action: %w", actionKey, err)
	}
	jobID, err := randomUUID()
	if err != nil {
		return err
	}
	idempotencyKey := workflowActionIdempotencyKey(workflowID, deliveryID, settlementID, actionKey)
	normalizedEventID := nullableString(deliveryID)
	settlementProvenanceID := nullableString(settlementID)
	provenanceActionKey := nullableString(actionKey)
	if deliveryID == "" && settlementID == "" {
		provenanceActionKey = nil
	}
	query := `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, normalized_event_id,
    agent_turn_settlement_id, action_key
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, COALESCE($5, clock_timestamp()), 3,
        $6, $7, $8, $9, $10, $11)
ON CONFLICT (normalized_event_id, action_key) WHERE normalized_event_id IS NOT NULL DO NOTHING`
	result, err := tx.Exec(ctx, query, jobID, WorkflowActionQueue, kind, payload, availableAt,
		idempotencyKey, workflowID, nullableString(attemptID), normalizedEventID,
		settlementProvenanceID, provenanceActionKey)
	if err != nil {
		return fmt.Errorf("enqueue %s action: %w", actionKey, err)
	}
	if result.RowsAffected() == 0 {
		var existingKind string
		var existingPayload []byte
		if err := tx.QueryRow(ctx, `SELECT kind, payload FROM jobs WHERE (normalized_event_id = $1 OR agent_turn_settlement_id = $2) AND action_key = $3`, nullableString(deliveryID), nullableString(settlementID), actionKey).Scan(&existingKind, &existingPayload); err != nil {
			return fmt.Errorf("verify %s action: %w", actionKey, err)
		}
		canonical, _ := canonicalJSON(payload)
		existingCanonical, _ := canonicalJSON(existingPayload)
		if existingKind != kind || !reflect.DeepEqual(canonical, existingCanonical) {
			return ErrJobIdempotencyConflict
		}
	}
	return nil
}

func workflowActionIdempotencyKey(workflowID, deliveryID, settlementID, actionKey string) string {
	if settlementID != "" {
		return fmt.Sprintf("workflow:%s:settlement:%s:action:%s", workflowID, settlementID, actionKey)
	}
	return fmt.Sprintf("workflow:%s:delivery:%s:action:%s", workflowID, deliveryID, actionKey)
}

func readWorkflowJobActionProvenance(ctx context.Context, tx pgx.Tx, jobID string) (string, string, error) {
	var deliveryID, settlementID string
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(normalized_event_id::text, ''),
       COALESCE(agent_turn_settlement_id::text, '')
FROM jobs WHERE id = $1`, jobID).Scan(&deliveryID, &settlementID); err != nil {
		return "", "", err
	}
	if (deliveryID == "") == (settlementID == "") {
		return "", "", ErrWorkflowDecisionInvalid
	}
	return deliveryID, settlementID, nil
}

func closeMutationAdmissionTx(ctx context.Context, tx pgx.Tx, guard workflow.TurnGuard) error {
	result, err := tx.Exec(ctx, `
UPDATE agent_turns AS turn
SET mutation_admission_open = FALSE,
    mutation_admission_closed_at = COALESCE(mutation_admission_closed_at, clock_timestamp()),
    status = 'CANCELLING'
FROM agent_sessions AS session, agent_assignments AS assignment
WHERE turn.id = $1 AND turn.agent_session_id = $2 AND turn.workflow_attempt_id = $3
  AND turn.execution_epoch = $4 AND turn.control_revision = $5 AND turn.active
  AND session.id = turn.agent_session_id AND assignment.id = session.agent_assignment_id
  AND assignment.role = $6`, guard.TurnID, guard.SessionID, guard.AttemptID,
		int64(guard.Epoch), int64(guard.ControlRevision), guard.Role)
	if err != nil {
		return fmt.Errorf("close mutation admission: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("close mutation admission: %w", ErrAgentTurnFenceLost)
	}
	if _, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = 'FAILED', finished_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Issue closed before mutation started'
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state = 'RESERVED' AND started_at IS NULL`, guard.TurnID, int64(guard.Epoch)); err != nil {
		return fmt.Errorf("fail unstarted closure mutations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = 'UNKNOWN', updated_at = clock_timestamp(),
    last_error = 'Issue closed with mutation in flight'
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state = 'IN_FLIGHT'`, guard.TurnID, int64(guard.Epoch)); err != nil {
		return fmt.Errorf("mark in-flight closure mutations unknown: %w", err)
	}
	return nil
}

func enqueueStopTurnJob(ctx context.Context, tx pgx.Tx, deliveryID, workflowID, closureID string, guard workflow.TurnGuard, revision uint64) (string, error) {
	var assignmentID string
	err := tx.QueryRow(ctx, `
SELECT session.agent_assignment_id::text
FROM agent_turns AS turn JOIN agent_sessions AS session ON session.id = turn.agent_session_id
WHERE turn.id = $1 AND turn.agent_session_id = $2 AND turn.workflow_attempt_id = $3
  AND turn.execution_epoch = $4 AND turn.control_revision = $5`, guard.TurnID, guard.SessionID,
		guard.AttemptID, int64(guard.Epoch), int64(guard.ControlRevision)).Scan(&assignmentID)
	if err != nil {
		return "", fmt.Errorf("resolve stopped turn hierarchy: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"workflow_id": workflowID, "workflow_attempt_id": guard.AttemptID, "closure_id": closureID,
		"turn_id": guard.TurnID, "session_id": guard.SessionID, "execution_epoch": guard.Epoch,
		"control_revision": guard.ControlRevision, "workflow_revision": revision,
	})
	if err != nil {
		return "", err
	}
	jobID, err := randomUUID()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, normalized_event_id, action_key
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', $5, clock_timestamp(), 3,
		$6, $7, $8, $9, $10, $11, $12, $13, 'stop-agent-turn')
ON CONFLICT (normalized_event_id, action_key) WHERE normalized_event_id IS NOT NULL DO NOTHING`,
		jobID, WorkflowActionQueue, StopAgentTurnJobKind, payload, stopAgentTurnJobPriority,
		fmt.Sprintf("workflow:%s:delivery:%s:action:stop-agent-turn", workflowID, deliveryID),
		workflowID, guard.AttemptID, assignmentID, guard.SessionID, guard.TurnID,
		int64(guard.Epoch), deliveryID)
	if err != nil {
		return "", fmt.Errorf("enqueue stop Agent Turn: %w", err)
	}
	return jobID, nil
}

type closureJobPayload struct {
	WorkflowID        string `json:"workflow_id"`
	WorkflowAttemptID string `json:"workflow_attempt_id,omitempty"`
	ClosureID         string `json:"closure_id"`
	TurnID            string `json:"turn_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ExecutionEpoch    int64  `json:"execution_epoch,omitempty"`
	ControlRevision   int64  `json:"control_revision,omitempty"`
	WorkflowRevision  int64  `json:"workflow_revision"`
}

func enqueueClosureSettlementJob(ctx context.Context, tx pgx.Tx, deliveryID, workflowID, attemptID, closureID string, revision uint64, guard *workflow.TurnGuard) (string, error) {
	payload := closureJobPayload{
		WorkflowID: workflowID, WorkflowAttemptID: attemptID, ClosureID: closureID,
		WorkflowRevision: int64(revision),
	}
	var assignmentID, sessionID, turnID string
	var epoch int64
	if guard != nil {
		payload.TurnID, payload.SessionID = guard.TurnID, guard.SessionID
		payload.ExecutionEpoch, payload.ControlRevision = int64(guard.Epoch), int64(guard.ControlRevision)
		sessionID, turnID, epoch = guard.SessionID, guard.TurnID, int64(guard.Epoch)
		if err := tx.QueryRow(ctx, `
SELECT agent_assignment_id::text FROM agent_sessions WHERE id = $1`, guard.SessionID).Scan(&assignmentID); err != nil {
			return "", fmt.Errorf("resolve closure settlement hierarchy: %w", err)
		}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode closure settlement job: %w", err)
	}
	jobID, err := randomUUID()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, normalized_event_id, action_key
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', $5, clock_timestamp(), 3,
        $6, $7, $8, $9, $10, $11, $12, $13, 'settle-closure')`,
		jobID, WorkflowActionQueue, SettleClosureJobKind, payloadJSON, settleClosureJobPriority,
		fmt.Sprintf("workflow:%s:closure:%s:settle", workflowID, closureID), workflowID,
		nullableString(attemptID), nullableString(assignmentID), nullableString(sessionID),
		nullableString(turnID), nullableEpoch(epoch), deliveryID)
	if err != nil {
		return "", fmt.Errorf("enqueue closure settlement: %w", err)
	}
	return jobID, nil
}

func createClosureBarrier(ctx context.Context, tx pgx.Tx, workflowID, closureID string, revision uint64, guard *workflow.TurnGuard, stopJobID, settlementJobID string) error {
	var turnID, sessionID, attemptID string
	var epoch, controlRevision int64
	var admissionClosedAt any
	if guard != nil {
		if stopJobID == "" {
			return errors.New("create closure barrier: source turn has no stop job")
		}
		turnID, sessionID, attemptID = guard.TurnID, guard.SessionID, guard.AttemptID
		epoch, controlRevision = int64(guard.Epoch), int64(guard.ControlRevision)
		var closedAt time.Time
		if err := tx.QueryRow(ctx, `
SELECT mutation_admission_closed_at
FROM agent_turns
WHERE id = $1 AND agent_session_id = $2 AND workflow_attempt_id = $3
  AND execution_epoch = $4 AND control_revision = $5 AND status = 'CANCELLING'
  AND NOT mutation_admission_open`, turnID, sessionID, attemptID, epoch, controlRevision).Scan(&closedAt); err != nil {
			return fmt.Errorf("verify closure mutation admission: %w", err)
		}
		admissionClosedAt = closedAt
	}
	_, err := tx.Exec(ctx, `
INSERT INTO workflow_closure_barriers (
    workflow_id, closure_id, workflow_revision, source_turn_id, source_session_id,
    source_attempt_id, source_execution_epoch, source_control_revision,
    mutation_admission_closed_at, stop_job_id, settlement_job_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, clock_timestamp()), $10, $11)`,
		workflowID, closureID, int64(revision), nullableString(turnID), nullableString(sessionID),
		nullableString(attemptID), nullableEpoch(epoch), nullableEpoch(controlRevision),
		admissionClosedAt, nullableString(stopJobID), settlementJobID)
	if err != nil {
		return fmt.Errorf("create closure barrier: %w", err)
	}
	return nil
}

func recordChangeProposalReview(ctx context.Context, tx pgx.Tx, deliveryID, settlementID, workflowID string, action workflow.RecordReviewAction) error {
	var proposalID string
	if err := tx.QueryRow(ctx, `
SELECT id::text FROM change_proposals
WHERE workflow_id = $1 AND repository_id = (SELECT repository_id FROM workflows WHERE id = $1)
  AND pull_request_id = $2`, workflowID, action.Review.ChangeProposalID).Scan(&proposalID); err != nil {
		return fmt.Errorf("resolve reviewed change proposal: %w", err)
	}
	var repositoryID int64
	if err := tx.QueryRow(ctx, `SELECT repository_id FROM workflows WHERE id = $1`, workflowID).Scan(&repositoryID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `
INSERT INTO change_proposal_reviews (
    repository_id, review_id, review_node_id, change_proposal_id,
    actor_id, head_sha, accepted, normalized_event_id, agent_turn_settlement_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (repository_id, review_id) DO NOTHING`, repositoryID, action.Review.ID,
		action.Review.NodeID, proposalID, action.Review.ActorID, action.Review.HeadSHA,
		action.Accepted, nullableString(deliveryID), nullableString(settlementID))
	if err != nil {
		return fmt.Errorf("record change proposal review: %w", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	if settlementID != "" {
		return errors.New("change proposal review already has different provenance")
	}
	var nodeID, existingProposalID, headSHA string
	var actorID int64
	var accepted bool
	if err := tx.QueryRow(ctx, `
SELECT review_node_id, change_proposal_id::text, actor_id, head_sha, accepted
FROM change_proposal_reviews WHERE repository_id = $1 AND review_id = $2`, repositoryID,
		action.Review.ID).Scan(&nodeID, &existingProposalID, &actorID, &headSHA, &accepted); err != nil {
		return err
	}
	if nodeID != action.Review.NodeID || existingProposalID != proposalID || actorID != action.Review.ActorID || headSHA != action.Review.HeadSHA || accepted != action.Accepted {
		return errors.New("change proposal review identity conflict")
	}
	return nil
}
