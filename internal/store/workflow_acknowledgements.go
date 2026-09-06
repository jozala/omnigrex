package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

// PendingEventSuccessorIntent is the single fallback intent retained by reconciliation.
// Phase 5 acknowledges the intent durably but leaves its execution to a future worker.
type PendingEventSuccessorIntent struct {
	Role            workflow.Role
	Purpose         workflow.TurnPurpose
	ExpectedHeadSHA string
	RetryOfTurnID   string
}

// PendingEventReconciliation is the durable result of consuming linked deferred events.
type PendingEventReconciliation struct {
	JobID          string
	WorkflowID     string
	SourceTurnID   string
	CompletedCount uint32
	Successor      PendingEventSuccessorIntent
	SuccessorJobID string
}

// AcknowledgePendingEventReconciliationFailure retries within the source job budget.
// Terminal failure preserves linked deferred events for a barrier-safe escalation authority.
func (store *Store) AcknowledgePendingEventReconciliationFailure(ctx context.Context, lease JobLease, cause error, retryable bool, retryDelay time.Duration) (WorkflowActionFailureAcknowledgement, error) {
	if err := validateWorkflowActionFailure(cause, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("begin pending-event reconciliation failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockFencedWorkflowJob(ctx, tx, lease, ReconcilePendingEventsJobKind, ErrPendingEventReconciliationFenceLost)
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	retryScheduled := retryable && job.AttemptCount < job.MaxAttempts
	if err := failWorkflowActionJobTx(ctx, tx, job, cause.Error(), retryable, retryScheduled, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, ErrPendingEventReconciliationFenceLost
	}
	acknowledgement := WorkflowActionFailureAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, RetryScheduled: retryScheduled,
	}
	if retryScheduled {
		if err := tx.Commit(ctx); err != nil {
			return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("commit pending-event reconciliation retry: %w", err)
		}
		return acknowledgement, nil
	}
	var payload pendingEventReconciliationPayload
	_ = json.Unmarshal(job.Payload, &payload)
	role := payload.FallbackRole
	if role != workflow.RoleDeveloper && role != workflow.RoleReviewer {
		if err := tx.QueryRow(ctx, `SELECT role FROM agent_assignments WHERE id = $1 AND workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(&role); err != nil {
			return WorkflowActionFailureAcknowledgement{}, ErrPendingEventReconciliationFenceLost
		}
	}
	escalationScheduled, err := requestWorkflowActionFailureEscalationTx(ctx, tx, job, role, cause.Error())
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	acknowledgement.EscalationScheduled = escalationScheduled
	if err := tx.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, job.WorkflowID).Scan(&acknowledgement.WorkflowRevision); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("commit pending-event reconciliation exhaustion: %w", err)
	}
	return acknowledgement, nil
}

type pendingEventReconciliationPayload struct {
	WorkflowID                string               `json:"workflow_id"`
	WorkflowAttemptID         string               `json:"workflow_attempt_id"`
	Count                     uint32               `json:"count"`
	LatestObservedHeadSHA     string               `json:"latest_observed_head_sha"`
	FallbackRole              workflow.Role        `json:"fallback_role"`
	FallbackPurpose           workflow.TurnPurpose `json:"fallback_purpose"`
	FallbackExpectedHeadSHA   string               `json:"fallback_expected_head_sha"`
	RetryOfTurnID             string               `json:"retry_of_turn_id"`
	Revision                  int64                `json:"revision"`
	SourceTurnID              string               `json:"source_turn_id"`
	SourceExecutionEpoch      int64                `json:"source_execution_epoch"`
	SourceControlRevision     int64                `json:"source_control_revision"`
	DeferredNormalizedEventID []string             `json:"deferred_normalized_event_ids"`
}

// AcknowledgePendingEventReconciliation replays every exactly linked deferred event
// and commits their outcomes, at most one coalesced successor, and job success atomically.
func (store *Store) AcknowledgePendingEventReconciliation(ctx context.Context, lease JobLease, factory PendingTransitionFactory) (PendingEventReconciliation, error) {
	if factory == nil {
		return PendingEventReconciliation{}, fmt.Errorf("acknowledge pending-event reconciliation: transition factory is nil")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PendingEventReconciliation{}, fmt.Errorf("begin pending-event reconciliation acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := lockFencedWorkflowJob(ctx, tx, lease, ReconcilePendingEventsJobKind, ErrPendingEventReconciliationFenceLost)
	if err != nil {
		return PendingEventReconciliation{}, err
	}
	var payload pendingEventReconciliationPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || !validPendingEventReconciliationPayload(payload, job) {
		return PendingEventReconciliation{}, ErrPendingEventReconciliationPayloadInvalid
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1 FOR UPDATE`, job.WorkflowID).Scan(&revision); err != nil || revision != payload.Revision {
		return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
	}
	var workflowID, attemptID, assignmentID, sessionID string
	var epoch, controlRevision int64
	var active bool
	if err := tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, turn.workflow_attempt_id::text,
       assignment.id::text, session.id::text, turn.execution_epoch,
       turn.control_revision, turn.active
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE turn.id = $1 FOR UPDATE`, payload.SourceTurnID).Scan(
		&workflowID, &attemptID, &assignmentID, &sessionID, &epoch, &controlRevision, &active,
	); err != nil || workflowID != job.WorkflowID || attemptID != job.WorkflowAttemptID ||
		assignmentID != job.AgentAssignmentID || sessionID != job.AgentSessionID ||
		epoch != job.ExecutionEpoch || controlRevision != payload.SourceControlRevision || active {
		return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
	}

	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return PendingEventReconciliation{}, fmt.Errorf("rehydrate pending-event reconciliation Workflow: %w", err)
	}
	if snapshot.Revision != uint64(payload.Revision) || snapshot.ActiveTurn != nil {
		return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
	}
	if live, err := hasLiveWorkflowSuccessor(ctx, tx, job.WorkflowID); err != nil {
		return PendingEventReconciliation{}, err
	} else if live {
		return PendingEventReconciliation{}, ErrWorkflowSuccessorConflict
	}
	linkedEvents, err := lockLinkedDeferredEvents(ctx, tx, job)
	if err != nil || !equalStringSets(linkedEventIDs(linkedEvents), payload.DeferredNormalizedEventID) || len(linkedEvents) != int(payload.Count) {
		return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
	}
	if err := prepareLinkedDeferredEvents(linkedEvents, snapshot); err != nil {
		return PendingEventReconciliation{}, err
	}

	intent := preparedTurnIntent{
		mode: workflow.AssignmentGenerationCurrent,
		turn: workflow.EnqueueTurnAction{
			Role: payload.FallbackRole, Purpose: payload.FallbackPurpose,
			ExpectedHeadSHA: payload.FallbackExpectedHeadSHA, RetryOfTurnID: payload.RetryOfTurnID,
		},
		set: payload.FallbackRole != "",
	}
	remaining := append([]linkedDeferredEvent(nil), linkedEvents...)
	for len(remaining) > 0 {
		next := nextLinkedDeferredEvent(remaining)
		linked := remaining[next]
		remaining = append(remaining[:next], remaining[next+1:]...)
		locator, transition, err := factory(linked.record)
		if err != nil {
			return PendingEventReconciliation{}, fmt.Errorf("build reconciled transition %s: %w", linked.record.DeliveryID, err)
		}
		if transition == nil {
			return PendingEventReconciliation{}, fmt.Errorf("build reconciled transition %s: transition is nil", linked.record.DeliveryID)
		}
		if err := validateWorkflowLocator(locator, linked.envelope); err != nil {
			return PendingEventReconciliation{}, err
		}
		resolvedWorkflowID, err := resolveWorkflowID(ctx, tx, locator)
		if err != nil {
			return PendingEventReconciliation{}, err
		}
		if resolvedWorkflowID != job.WorkflowID {
			return PendingEventReconciliation{}, ErrWorkflowLocatorMismatch
		}
		decision := transition(snapshot)
		if err := validateWorkflowDecision(snapshot, decision); err != nil {
			return PendingEventReconciliation{}, err
		}
		if decision.Disposition == workflow.DispositionDeferred {
			return PendingEventReconciliation{}, ErrWorkflowDecisionInvalid
		}
		persistedDecision, replayIntent, err := withoutSuccessorActions(decision)
		if err != nil {
			return PendingEventReconciliation{}, err
		}
		if decision.Disposition == workflow.DispositionApplied {
			if replayIntent.set {
				intent = replayIntent
			} else if !workflowStateAllowsSuccessor(decision.Snapshot.State) {
				intent = preparedTurnIntent{mode: workflow.AssignmentGenerationCurrent}
			}
			if err := persistAppliedDecision(ctx, tx, linked.record.DeliveryID, job.WorkflowID, snapshot, persistedDecision, ""); err != nil {
				return PendingEventReconciliation{}, err
			}
		}
		result, err := tx.Exec(ctx, `
UPDATE normalized_events
SET status = 'COMPLETED', disposition = $2, reason = $3, applied_revision = $4,
    deferred_for_turn_id = NULL, processed_at = clock_timestamp()
WHERE delivery_id = $1 AND workflow_id = $5 AND deferred_for_turn_id = $6
  AND status = 'DEFERRED'`, linked.record.DeliveryID, decision.Disposition, decision.Reason,
			int64(decision.Snapshot.Revision), job.WorkflowID, job.AgentTurnID)
		if err != nil {
			return PendingEventReconciliation{}, fmt.Errorf("complete reconciled event %s: %w", linked.record.DeliveryID, err)
		}
		if result.RowsAffected() != 1 {
			return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
		}
		snapshot = decision.Snapshot
	}
	if err := normalizeSuccessorIntent(snapshot, &intent); err != nil {
		return PendingEventReconciliation{}, err
	}
	if live, err := hasLiveWorkflowSuccessor(ctx, tx, job.WorkflowID); err != nil {
		return PendingEventReconciliation{}, err
	} else if live {
		return PendingEventReconciliation{}, ErrWorkflowSuccessorConflict
	}

	successor := PendingEventSuccessorIntent{
		Role: intent.turn.Role, Purpose: intent.turn.Purpose,
		ExpectedHeadSHA: intent.turn.ExpectedHeadSHA, RetryOfTurnID: intent.turn.RetryOfTurnID,
	}
	successorJobID := ""
	if intent.set {
		normalizedEventID, settlementID, err := readWorkflowJobActionProvenance(ctx, tx, job.ID)
		if err != nil {
			return PendingEventReconciliation{}, ErrPendingEventReconciliationFenceLost
		}
		if err := enqueueWorkflowJobWithProvenance(ctx, tx, normalizedEventID, settlementID, job.WorkflowID, job.WorkflowAttemptID,
			"prepare-agent-turn", PrepareAgentTurnJobKind, map[string]any{
				"mode": intent.mode, "role": successor.Role,
				"purpose": successor.Purpose, "expected_head_sha": successor.ExpectedHeadSHA,
				"retry_of_turn_id": successor.RetryOfTurnID, "revision": snapshot.Revision,
			}, nil); err != nil {
			return PendingEventReconciliation{}, err
		}
		if err := tx.QueryRow(ctx, `
SELECT id::text FROM jobs
WHERE (normalized_event_id = $1 OR agent_turn_settlement_id = $2)
  AND action_key = 'prepare-agent-turn'`, nullableString(normalizedEventID),
			nullableString(settlementID)).Scan(&successorJobID); err != nil {
			return PendingEventReconciliation{}, fmt.Errorf("resolve reconciled successor job: %w", err)
		}
	}
	jobResult, err := json.Marshal(map[string]any{
		"completed_count":  payload.Count,
		"final_revision":   snapshot.Revision,
		"successor_job_id": nullableString(successorJobID),
		"successor": map[string]any{
			"role": successor.Role, "purpose": successor.Purpose,
			"expected_head_sha": successor.ExpectedHeadSHA, "retry_of_turn_id": successor.RetryOfTurnID,
		},
	})
	if err != nil {
		return PendingEventReconciliation{}, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, jobResult, ErrPendingEventReconciliationFenceLost); err != nil {
		return PendingEventReconciliation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PendingEventReconciliation{}, fmt.Errorf("commit pending-event reconciliation acknowledgement: %w", err)
	}
	return PendingEventReconciliation{
		JobID: job.ID, WorkflowID: job.WorkflowID, SourceTurnID: job.AgentTurnID,
		CompletedCount: payload.Count, Successor: successor, SuccessorJobID: successorJobID,
	}, nil
}

func validPendingEventReconciliationPayload(payload pendingEventReconciliationPayload, job Job) bool {
	if payload.WorkflowID != job.WorkflowID || payload.WorkflowAttemptID != job.WorkflowAttemptID ||
		payload.SourceTurnID != job.AgentTurnID || payload.SourceExecutionEpoch != job.ExecutionEpoch ||
		payload.SourceControlRevision <= 0 || payload.Revision <= 0 || payload.Count == 0 ||
		len(payload.DeferredNormalizedEventID) != int(payload.Count) {
		return false
	}
	seen := make(map[string]struct{}, len(payload.DeferredNormalizedEventID))
	for _, eventID := range payload.DeferredNormalizedEventID {
		if !validUUID(eventID) {
			return false
		}
		if _, duplicate := seen[eventID]; duplicate {
			return false
		}
		seen[eventID] = struct{}{}
	}
	if payload.FallbackRole == "" {
		return payload.FallbackPurpose == "" && payload.FallbackExpectedHeadSHA == "" && payload.RetryOfTurnID == ""
	}
	if payload.FallbackRole != workflow.RoleDeveloper && payload.FallbackRole != workflow.RoleReviewer {
		return false
	}
	switch payload.FallbackPurpose {
	case workflow.TurnPurposeInitialDevelopment, workflow.TurnPurposeReview,
		workflow.TurnPurposeRequestedChanges, workflow.TurnPurposeRetry,
		workflow.TurnPurposeSynchronization, workflow.TurnPurposeReactivation:
	default:
		return false
	}
	return payload.RetryOfTurnID == "" || validUUID(payload.RetryOfTurnID)
}

type linkedDeferredEvent struct {
	record          NormalizedEventRecord
	envelope        workflowEnvelope
	synchronization *linkedSynchronization
}

type linkedSynchronization struct {
	beforeSHA string
	headSHA   string
	order     int
}

func lockLinkedDeferredEvents(ctx context.Context, tx pgx.Tx, job Job) ([]linkedDeferredEvent, error) {
	rows, err := tx.Query(ctx, `
SELECT event.delivery_id::text, event.payload, event.status,
       COALESCE(event.workflow_id::text, ''), event.disposition, event.reason,
       event.applied_revision, COALESCE(event.deferred_for_turn_id::text, ''),
       event.created_at, event.processed_at,
       delivery.status, delivery.event_name, COALESCE(delivery.action, ''),
       COALESCE(delivery.repository_id, 0),
       COALESCE(delivery.repository_owner, ''), COALESCE(delivery.repository_name, ''),
       COALESCE(delivery.issue_id, 0), COALESCE(delivery.issue_number, 0)
FROM job_normalized_events AS link
JOIN normalized_events AS event ON event.delivery_id = link.normalized_event_id
JOIN webhook_deliveries AS delivery USING (delivery_id)
WHERE link.job_id = $1
ORDER BY event.created_at, event.delivery_id
FOR UPDATE OF event, delivery`, job.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var linked []linkedDeferredEvent
	for rows.Next() {
		var item linkedDeferredEvent
		var payload []byte
		var webhookStatus WebhookStatus
		if err := rows.Scan(
			&item.record.DeliveryID, &payload, &item.record.Status,
			&item.record.WorkflowID, &item.record.Disposition, &item.record.Reason,
			&item.record.AppliedRevision, &item.record.DeferredForTurnID,
			&item.record.CreatedAt, &item.record.ProcessedAt,
			&webhookStatus, &item.envelope.eventName, &item.envelope.action,
			&item.envelope.repositoryID, &item.envelope.repositoryOwner,
			&item.envelope.repositoryName, &item.envelope.issueID, &item.envelope.issueNumber,
		); err != nil {
			return nil, err
		}
		item.record.Payload = json.RawMessage(payload)
		if item.record.Status != NormalizedEventDeferred || item.record.WorkflowID != job.WorkflowID ||
			item.record.DeferredForTurnID != job.AgentTurnID || webhookStatus != WebhookProcessed {
			return nil, ErrPendingEventReconciliationFenceLost
		}
		linked = append(linked, item)
	}
	return linked, rows.Err()
}

// prepareLinkedDeferredEvents validates synchronization identity and derives one
// causal path before payload content can influence replay order.
func prepareLinkedDeferredEvents(linked []linkedDeferredEvent, snapshot workflow.Snapshot) error {
	for index := range linked {
		item := &linked[index]
		if err := validateNormalizedDeliveryID(item.record.Payload, item.record.DeliveryID); err != nil {
			return fmt.Errorf("%w: reconcile normalized event %s: %v", ErrPendingNormalizedEventInvalid, item.record.DeliveryID, err)
		}
		var normalized struct {
			DeliveryID string `json:"delivery_id"`
			EventName  string `json:"event"`
			Action     string `json:"action"`
			Repository struct {
				ID    int64  `json:"id"`
				Owner string `json:"owner"`
				Name  string `json:"name"`
			} `json:"repository"`
			PullRequest *struct {
				ID        int64  `json:"id"`
				Number    int64  `json:"number"`
				HeadSHA   string `json:"head_sha"`
				BeforeSHA string `json:"before_sha"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(item.record.Payload, &normalized); err != nil {
			return fmt.Errorf("%w: decode normalized event %s: %v", ErrPendingNormalizedEventInvalid, item.record.DeliveryID, err)
		}
		durableSynchronization := item.envelope.eventName == "pull_request" && item.envelope.action == "synchronize"
		payloadSynchronization := normalized.EventName == "pull_request" && normalized.Action == "synchronize"
		if !durableSynchronization && !payloadSynchronization {
			continue
		}
		pullRequest := normalized.PullRequest
		valid := durableSynchronization && payloadSynchronization &&
			normalized.DeliveryID == item.record.DeliveryID &&
			normalized.Repository.ID > 0 && normalized.Repository.ID == item.envelope.repositoryID &&
			strings.TrimSpace(normalized.Repository.Owner) != "" && normalized.Repository.Owner == item.envelope.repositoryOwner &&
			strings.TrimSpace(normalized.Repository.Name) != "" && normalized.Repository.Name == item.envelope.repositoryName &&
			pullRequest != nil && pullRequest.ID > 0 && pullRequest.Number > 0 &&
			strings.TrimSpace(pullRequest.BeforeSHA) != "" && strings.TrimSpace(pullRequest.HeadSHA) != "" &&
			snapshot.ChangeProposal != nil && pullRequest.ID == snapshot.ChangeProposal.ID && pullRequest.Number == snapshot.ChangeProposal.Number
		if !valid {
			return fmt.Errorf("%w: synchronization identity %s contradicts its durable envelope or Workflow", ErrPendingNormalizedEventInvalid, item.record.DeliveryID)
		}
		item.synchronization = &linkedSynchronization{beforeSHA: pullRequest.BeforeSHA, headSHA: pullRequest.HeadSHA}
	}
	return prepareLinkedSynchronizationOrder(linked, snapshot)
}

type linkedSynchronizationEdge struct {
	beforeSHA string
	headSHA   string
}

func prepareLinkedSynchronizationOrder(linked []linkedDeferredEvent, snapshot workflow.Snapshot) error {
	outgoing := make(map[string]linkedSynchronizationEdge)
	incoming := make(map[string]linkedSynchronizationEdge)
	edges := make(map[linkedSynchronizationEdge]struct{})
	for index := range linked {
		synchronization := linked[index].synchronization
		if synchronization == nil {
			continue
		}
		edge := linkedSynchronizationEdge{beforeSHA: synchronization.beforeSHA, headSHA: synchronization.headSHA}
		if edge.beforeSHA == edge.headSHA {
			return fmt.Errorf("%w: synchronization cycle at delivery %s", ErrPendingEventCausalGap, linked[index].record.DeliveryID)
		}
		if _, duplicate := edges[edge]; duplicate {
			continue
		}
		if previous, exists := outgoing[edge.beforeSHA]; exists && previous != edge {
			return fmt.Errorf("%w: synchronization fork at head %q", ErrPendingEventCausalGap, edge.beforeSHA)
		}
		if previous, exists := incoming[edge.headSHA]; exists && previous != edge {
			return fmt.Errorf("%w: ambiguous synchronization predecessor for head %q", ErrPendingEventCausalGap, edge.headSHA)
		}
		edges[edge] = struct{}{}
		outgoing[edge.beforeSHA] = edge
		incoming[edge.headSHA] = edge
	}
	if len(edges) == 0 {
		return nil
	}
	if snapshot.ChangeProposal == nil {
		return fmt.Errorf("%w: Workflow has no Change Proposal", ErrPendingEventCausalGap)
	}

	root := ""
	for index := range linked {
		synchronization := linked[index].synchronization
		if synchronization == nil {
			continue
		}
		if _, hasPredecessor := incoming[synchronization.beforeSHA]; hasPredecessor {
			continue
		}
		if root != "" && root != synchronization.beforeSHA {
			return fmt.Errorf("%w: disconnected synchronization chains", ErrPendingEventCausalGap)
		}
		root = synchronization.beforeSHA
	}
	if root == "" {
		return fmt.Errorf("%w: synchronization cycle", ErrPendingEventCausalGap)
	}

	orders := make(map[linkedSynchronizationEdge]int, len(edges))
	nodes := map[string]struct{}{root: {}}
	for head, order := root, 0; ; order++ {
		edge, exists := outgoing[head]
		if !exists {
			break
		}
		if _, visited := nodes[edge.headSHA]; visited {
			return fmt.Errorf("%w: synchronization cycle at head %q", ErrPendingEventCausalGap, edge.headSHA)
		}
		orders[edge] = order
		nodes[edge.headSHA] = struct{}{}
		head = edge.headSHA
	}
	if len(orders) != len(edges) {
		return fmt.Errorf("%w: disconnected synchronization chains", ErrPendingEventCausalGap)
	}
	currentHead := snapshot.ChangeProposal.HeadSHA
	if _, connected := nodes[currentHead]; !connected {
		return fmt.Errorf("%w after head %q", ErrPendingEventCausalGap, currentHead)
	}
	for index := range linked {
		if synchronization := linked[index].synchronization; synchronization != nil {
			synchronization.order = orders[linkedSynchronizationEdge{beforeSHA: synchronization.beforeSHA, headSHA: synchronization.headSHA}]
		}
	}
	return nil
}

// Non-synchronization events retain arrival order. Once a synchronization is
// next, its oldest remaining causal edge, including exact redeliveries, wins.
func nextLinkedDeferredEvent(remaining []linkedDeferredEvent) int {
	if len(remaining) == 0 || remaining[0].synchronization == nil {
		return 0
	}
	next := 0
	for index := 1; index < len(remaining); index++ {
		candidate := remaining[index].synchronization
		if candidate != nil && candidate.order < remaining[next].synchronization.order {
			next = index
		}
	}
	return next
}

func linkedEventIDs(linked []linkedDeferredEvent) []string {
	ids := make([]string, len(linked))
	for index := range linked {
		ids[index] = linked[index].record.DeliveryID
	}
	return ids
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func withoutSuccessorActions(decision workflow.Decision) (workflow.Decision, preparedTurnIntent, error) {
	intent := preparedTurnIntent{mode: workflow.AssignmentGenerationCurrent}
	actions := make([]workflow.Action, 0, len(decision.Actions))
	for _, action := range decision.Actions {
		switch action := action.(type) {
		case workflow.EnsureAssignmentsAction:
			intent.mode = action.Mode
		case workflow.EnqueueTurnAction:
			if intent.set {
				return workflow.Decision{}, preparedTurnIntent{}, ErrWorkflowDecisionInvalid
			}
			intent.turn, intent.set = action, true
		case workflow.ReconcilePendingEventsAction:
			return workflow.Decision{}, preparedTurnIntent{}, ErrWorkflowDecisionInvalid
		default:
			actions = append(actions, action)
		}
	}
	decision.Actions = actions
	return decision, intent, nil
}

func workflowStateAllowsSuccessor(state workflow.State) bool {
	return state == workflow.StateDeveloping || state == workflow.StateReviewing
}

func normalizeSuccessorIntent(snapshot workflow.Snapshot, intent *preparedTurnIntent) error {
	if !workflowStateAllowsSuccessor(snapshot.State) {
		*intent = preparedTurnIntent{mode: workflow.AssignmentGenerationCurrent}
		return nil
	}
	if !intent.set {
		return nil
	}
	expectedRole := workflow.RoleDeveloper
	if snapshot.State == workflow.StateReviewing {
		expectedRole = workflow.RoleReviewer
	}
	if intent.turn.Role != expectedRole {
		return ErrWorkflowDecisionInvalid
	}
	if snapshot.ChangeProposal == nil {
		if expectedRole == workflow.RoleReviewer {
			return ErrWorkflowDecisionInvalid
		}
		intent.turn.ExpectedHeadSHA = ""
	} else {
		intent.turn.ExpectedHeadSHA = snapshot.ChangeProposal.HeadSHA
	}
	return nil
}

func hasLiveWorkflowSuccessor(ctx context.Context, tx pgx.Tx, workflowID string) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM agent_turns AS turn
    JOIN agent_sessions AS session ON session.id = turn.agent_session_id
    JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
    WHERE assignment.workflow_id = $1 AND turn.active
) OR EXISTS (
    SELECT 1 FROM jobs
    WHERE workflow_id = $1 AND kind = $2 AND status IN ('AVAILABLE', 'LEASED')
) OR EXISTS (
    SELECT 1 FROM agent_turns
    WHERE workflow_id = $1 AND recovery_started_at IS NOT NULL AND recovery_settled_at IS NULL
)`, workflowID, PrepareAgentTurnJobKind).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("check live Workflow successor: %w", err)
	}
	return live, nil
}

// ClosureSettlement exposes the durable closure barrier state needed to emit a
// future ClosureSettledEvent after this Phase 5 acknowledgement succeeds.
type ClosureSettlement struct {
	WorkflowID              string
	ClosureID               string
	WorkflowRevision        int64
	CurrentWorkflowRevision int64
	SourceTurnID            string
	ExecutionEpoch          int64
	ControlRevision         int64
	RuntimeStoppedAt        *time.Time
	SettledAt               *time.Time
}

// GetClosureTurnRuntimeIdentity returns the complete Runtime Process identity
// under the exact live closure stop-job fence.
func (store *Store) GetClosureTurnRuntimeIdentity(ctx context.Context, lease JobLease) (AgentTurnRuntimeIdentity, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRuntimeIdentity{}, fmt.Errorf("begin closure Runtime Process identity read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return AgentTurnRuntimeIdentity{}, err
	}
	job, _, barrier, err := lockClosureJob(ctx, tx, lease, StopAgentTurnJobKind)
	if err != nil {
		return AgentTurnRuntimeIdentity{}, err
	}
	if barrier.SourceTurnID == "" {
		return AgentTurnRuntimeIdentity{}, ErrClosureSettlementFenceLost
	}
	identity := AgentTurnRuntimeIdentity{
		AssignmentID: job.AgentAssignmentID, AgentSessionID: job.AgentSessionID,
		AgentTurnID: job.AgentTurnID, ExecutionEpoch: job.ExecutionEpoch,
	}
	var assignmentProfileName, assignmentProfileVersion string
	if err := tx.QueryRow(ctx, `
SELECT session.runtime_profile_name, session.runtime_profile_version,
       assignment.runtime_profile_name, assignment.runtime_profile_version
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE assignment.id = $1 AND session.id = $2 AND assignment.workflow_id = $3`,
		job.AgentAssignmentID, job.AgentSessionID, job.WorkflowID).Scan(
		&identity.RuntimeProfileName, &identity.RuntimeProfileVersion,
		&assignmentProfileName, &assignmentProfileVersion,
	); err != nil || identity.RuntimeProfileName != assignmentProfileName || identity.RuntimeProfileVersion != assignmentProfileVersion {
		return AgentTurnRuntimeIdentity{}, ErrClosureSettlementFenceLost
	}
	if err := validateAgentTurnRuntimeIdentity(identity); err != nil {
		return AgentTurnRuntimeIdentity{}, ErrClosureSettlementFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRuntimeIdentity{}, fmt.Errorf("commit closure Runtime Process identity read: %w", err)
	}
	return identity, nil
}

// GetClosureMutationReconciliationContext returns fresh external-read scope
// after the exact closure Runtime Process stop is durable.
func (store *Store) GetClosureMutationReconciliationContext(ctx context.Context, lease JobLease) (AgentTurnMutationReconciliationContext, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("begin closure mutation reconciliation context: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return AgentTurnMutationReconciliationContext{}, err
	}
	job, _, barrier, err := lockClosureJob(ctx, tx, lease, SettleClosureJobKind)
	if err != nil {
		return AgentTurnMutationReconciliationContext{}, err
	}
	if barrier.SourceTurnID == "" {
		return AgentTurnMutationReconciliationContext{}, ErrClosureSettlementFenceLost
	}
	if err := requireClosureRuntimeStopped(ctx, tx, barrier); err != nil {
		return AgentTurnMutationReconciliationContext{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, barrier.SourceTurnID)
	if err != nil || turn.AgentSessionID != barrier.SourceSessionID ||
		turn.WorkflowAttemptID != barrier.SourceAttemptID || turn.ExecutionEpoch != barrier.ExecutionEpoch ||
		turn.ControlRevision != barrier.ControlRevision {
		return AgentTurnMutationReconciliationContext{}, ErrClosureSettlementFenceLost
	}
	turn.AgentAssignmentID = job.AgentAssignmentID
	reconciliation := AgentTurnMutationReconciliationContext{WorkflowID: job.WorkflowID, Turn: turn.AgentTurn}
	if err := tx.QueryRow(ctx, `
SELECT repository_id, repository_owner, repository_name, issue_id, issue_number
FROM workflows WHERE id = $1`, job.WorkflowID).Scan(
		&reconciliation.Repository.ID, &reconciliation.Repository.Owner,
		&reconciliation.Repository.Name, &reconciliation.Issue.ID,
		&reconciliation.Issue.Number,
	); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("read closure mutation Work Item: %w", err)
	}
	if err := tx.QueryRow(ctx, `
SELECT source.role, COALESCE(reviewer.github_app_actor_id, 0)
FROM agent_assignments AS source
LEFT JOIN agent_assignments AS reviewer
  ON reviewer.workflow_id = source.workflow_id
 AND reviewer.role = 'REVIEWER'
 AND reviewer.status <> 'SUPERSEDED'
 AND reviewer.state_deleted_at IS NULL
WHERE source.id = $1 AND source.workflow_id = $2`, job.AgentAssignmentID, job.WorkflowID).Scan(
		&reconciliation.Role, &reconciliation.ReviewerActorID,
	); err != nil || reconciliation.Role != workflow.RoleDeveloper && reconciliation.Role != workflow.RoleReviewer {
		return AgentTurnMutationReconciliationContext{}, ErrClosureSettlementFenceLost
	}
	if turn.ChangeProposalID != "" {
		proposal := &AgentTurnChangeProposal{}
		if err := tx.QueryRow(ctx, `
SELECT id::text, pull_request_id, pull_request_number, base_ref, base_sha, head_ref, head_sha
FROM change_proposals
WHERE id = $1 AND workflow_id = $2 AND repository_id = $3
  AND repository_owner = $4 AND repository_name = $5`,
			turn.ChangeProposalID, job.WorkflowID, reconciliation.Repository.ID,
			reconciliation.Repository.Owner, reconciliation.Repository.Name,
		).Scan(
			&proposal.ID, &proposal.PullRequestID, &proposal.PullRequestNumber,
			&proposal.BaseRef, &proposal.BaseSHA, &proposal.HeadRef, &proposal.HeadSHA,
		); err != nil {
			return AgentTurnMutationReconciliationContext{}, ErrClosureSettlementFenceLost
		}
		reconciliation.ChangeProposal = proposal
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnMutationReconciliationContext{}, fmt.Errorf("commit closure mutation reconciliation context: %w", err)
	}
	return reconciliation, nil
}

// ListClosureMutationsForReconciliation returns only ambiguous admitted
// mutations for the exact closure source Turn epoch, in invocation order.
func (store *Store) ListClosureMutationsForReconciliation(ctx context.Context, lease JobLease) ([]MutationReservation, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin closure mutation reconciliation list: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return nil, err
	}
	_, _, barrier, err := lockClosureJob(ctx, tx, lease, SettleClosureJobKind)
	if err != nil {
		return nil, err
	}
	if barrier.SourceTurnID == "" {
		return nil, ErrClosureSettlementFenceLost
	}
	if err := requireClosureRuntimeStopped(ctx, tx, barrier); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, mutationSelect+`
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')
ORDER BY invocation_number`, barrier.SourceTurnID, barrier.ExecutionEpoch)
	if err != nil {
		return nil, fmt.Errorf("list closure mutations for reconciliation: %w", err)
	}
	defer rows.Close()
	var mutations []MutationReservation
	for rows.Next() {
		mutation, err := scanMutation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan closure mutation for reconciliation: %w", err)
		}
		mutations = append(mutations, mutation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list closure mutations for reconciliation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit closure mutation reconciliation list: %w", err)
	}
	return mutations, nil
}

// AcknowledgeClosureActionFailure records one closure worker failure and preserves cleanup authority after escalation.
func (store *Store) AcknowledgeClosureActionFailure(ctx context.Context, lease JobLease, cause error, retryable bool, retryDelay time.Duration) (WorkflowActionFailureAcknowledgement, error) {
	return store.acknowledgeClosureActionFailure(ctx, lease, cause, retryable, retryDelay, true)
}

// AcknowledgeClosureSettlementWait releases a settlement lease with delay when
// its healthy stop dependency has not crossed the durable barrier yet.
func (store *Store) AcknowledgeClosureSettlementWait(ctx context.Context, lease JobLease, retryDelay time.Duration) (WorkflowActionFailureAcknowledgement, error) {
	return store.acknowledgeClosureActionFailure(ctx, lease, ErrClosureSettlementUnsettled, true, retryDelay, false)
}

func (store *Store) acknowledgeClosureActionFailure(ctx context.Context, lease JobLease, cause error, _ bool, retryDelay time.Duration, actualFailure bool) (WorkflowActionFailureAcknowledgement, error) {
	if err := validateWorkflowActionFailure(cause, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	expectedKind := lease.Kind
	if expectedKind != StopAgentTurnJobKind && expectedKind != SettleClosureJobKind {
		return WorkflowActionFailureAcknowledgement{}, ErrClosureSettlementFenceLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("begin closure action failure acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	job, _, _, err := lockClosureJob(ctx, tx, lease, expectedKind)
	if err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	retryScheduled := true
	acknowledgement := WorkflowActionFailureAcknowledgement{
		JobID: job.ID, WorkflowID: job.WorkflowID, RetryScheduled: retryScheduled,
	}
	if !actualFailure {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET max_attempts = max_attempts + 1 WHERE id = $1`, job.ID); err != nil {
			return WorkflowActionFailureAcknowledgement{}, err
		}
	} else if job.AttemptCount >= job.MaxAttempts {
		continued, scheduled, err := continueIrreversibleJobAfterExhaustionTx(ctx, tx, job)
		if err != nil {
			return WorkflowActionFailureAcknowledgement{}, err
		}
		if !continued {
			return WorkflowActionFailureAcknowledgement{}, ErrClosureSettlementFenceLost
		}
		acknowledgement.EscalationScheduled = scheduled
	}
	if err := failWorkflowActionJobTx(ctx, tx, job, cause.Error(), true, retryScheduled, retryDelay); err != nil {
		return WorkflowActionFailureAcknowledgement{}, ErrClosureSettlementFenceLost
	}
	if err := tx.QueryRow(ctx, `SELECT state_revision FROM workflows WHERE id = $1`, job.WorkflowID).Scan(&acknowledgement.WorkflowRevision); err != nil {
		return WorkflowActionFailureAcknowledgement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowActionFailureAcknowledgement{}, fmt.Errorf("commit closure action failure acknowledgement: %w", err)
	}
	return acknowledgement, nil
}

// AcknowledgeClosureTurnStopped records Runtime Process termination under the
// exact closure stop-job, source-turn, epoch, control, and Workflow revision fence.
func (store *Store) AcknowledgeClosureTurnStopped(ctx context.Context, lease JobLease) (ClosureSettlement, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ClosureSettlement{}, fmt.Errorf("begin closure stop acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return ClosureSettlement{}, err
	}
	job, payload, barrier, err := lockClosureJob(ctx, tx, lease, StopAgentTurnJobKind)
	if err != nil {
		return ClosureSettlement{}, err
	}
	if barrier.SourceTurnID == "" || barrier.StopJobID != job.ID {
		return ClosureSettlement{}, ErrClosureSettlementFenceLost
	}
	var stoppedAt time.Time
	if err := tx.QueryRow(ctx, `
UPDATE workflow_closure_barriers
SET runtime_stopped_at = COALESCE(runtime_stopped_at, clock_timestamp())
WHERE workflow_id = $1 AND closure_id = $2 AND settled_at IS NULL
RETURNING runtime_stopped_at`, payload.WorkflowID, payload.ClosureID).Scan(&stoppedAt); err != nil {
		return ClosureSettlement{}, ErrClosureSettlementFenceLost
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, json.RawMessage(`{"runtime_stopped":true}`), ErrClosureSettlementFenceLost); err != nil {
		return ClosureSettlement{}, err
	}
	barrier.RuntimeStoppedAt = &stoppedAt
	if err := tx.Commit(ctx); err != nil {
		return ClosureSettlement{}, fmt.Errorf("commit closure stop acknowledgement: %w", err)
	}
	return barrier.settlement(), nil
}

// ReconcileClosureMutation records a known terminal result for an ambiguous
// admitted mutation after the closure stop acknowledgement.
func (store *Store) ReconcileClosureMutation(ctx context.Context, lease JobLease, mutationID string, outcome RecoveredMutationOutcome) (MutationReservation, error) {
	result, err := validateReconciledMutationOutcome("reconcile closure mutation", outcome)
	if err != nil {
		return MutationReservation{}, err
	}
	if !validUUID(mutationID) {
		return MutationReservation{}, ErrMutationStateConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MutationReservation{}, fmt.Errorf("begin closure mutation reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return MutationReservation{}, err
	}
	_, _, barrier, err := lockClosureJob(ctx, tx, lease, SettleClosureJobKind)
	if err != nil {
		return MutationReservation{}, err
	}
	if barrier.SourceTurnID == "" || barrier.RuntimeStoppedAt == nil {
		return MutationReservation{}, ErrClosureSettlementUnsettled
	}
	update, err := tx.Exec(ctx, `
UPDATE tool_invocations
SET state = $4, result = $5, last_error = NULLIF($6, ''),
    updated_at = clock_timestamp(), finished_at = clock_timestamp(),
    duration_ms = CASE WHEN started_at IS NULL THEN NULL
        ELSE GREATEST(0, (EXTRACT(EPOCH FROM (clock_timestamp() - started_at)) * 1000)::bigint) END
WHERE id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 AND kind = 'MUTATION'
  AND state IN ('UNKNOWN', 'RECONCILING')`, mutationID, barrier.SourceTurnID,
		barrier.ExecutionEpoch, outcome.State, nullableJSON(result), outcome.LastError)
	if err != nil {
		return MutationReservation{}, fmt.Errorf("reconcile closure mutation: %w", err)
	}
	if update.RowsAffected() != 1 {
		return MutationReservation{}, ErrMutationStateConflict
	}
	mutation, err := getMutationByID(ctx, tx, mutationID)
	if err != nil {
		return MutationReservation{}, fmt.Errorf("read reconciled closure mutation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationReservation{}, fmt.Errorf("commit closure mutation reconciliation: %w", err)
	}
	return mutation, nil
}

// CompleteClosureSettlement atomically settles the exact closure barrier,
// applies the reducer-owned internal ClosureSettled transition, concretely
// retains current Assignments and Sessions, and succeeds the settlement Job.
func (store *Store) CompleteClosureSettlement(ctx context.Context, lease JobLease) (ClosureSettlement, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ClosureSettlement{}, fmt.Errorf("begin closure settlement completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClosureWorkflow(ctx, tx, lease); err != nil {
		return ClosureSettlement{}, err
	}
	if settled, ok, err := resolveCompletedClosureSettlement(ctx, tx, lease); err != nil || ok {
		if err != nil {
			return ClosureSettlement{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ClosureSettlement{}, fmt.Errorf("commit resolved closure settlement: %w", err)
		}
		return settled, nil
	}
	job, _, barrier, err := lockClosureJob(ctx, tx, lease, SettleClosureJobKind)
	if err != nil {
		return ClosureSettlement{}, err
	}
	if barrier.SourceTurnID != "" {
		if err := requireClosureRuntimeStopped(ctx, tx, barrier); err != nil {
			return ClosureSettlement{}, err
		}
		var unsettled bool
		if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, barrier.SourceTurnID, barrier.ExecutionEpoch).Scan(&unsettled); err != nil {
			return ClosureSettlement{}, fmt.Errorf("check closure mutations: %w", err)
		}
		if unsettled {
			return ClosureSettlement{}, ErrClosureSettlementUnsettled
		}
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, barrier.WorkflowID)
	if err != nil {
		return ClosureSettlement{}, fmt.Errorf("rehydrate closure settlement Workflow: %w", err)
	}
	if snapshot.State != workflow.StateClosing || snapshot.Closure == nil || snapshot.Closure.ID != barrier.ClosureID ||
		(barrier.SourceTurnID == "") != (snapshot.ActiveTurn == nil) {
		return ClosureSettlement{}, fmt.Errorf("closure settlement aggregate does not match barrier: %w", ErrClosureSettlementFenceLost)
	}
	if err := cancelPendingAgentTurnPreparationsTx(ctx, tx, barrier.WorkflowID); err != nil {
		return ClosureSettlement{}, err
	}
	var assignmentsExist bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_assignments
    WHERE workflow_id = $1 AND status <> 'SUPERSEDED' AND state_deleted_at IS NULL
)`, barrier.WorkflowID).Scan(&assignmentsExist); err != nil {
		return ClosureSettlement{}, fmt.Errorf("inspect closure Assignments: %w", err)
	}
	var turn *workflow.TurnGuard
	if snapshot.ActiveTurn != nil {
		if snapshot.ActiveTurn.ID != barrier.SourceTurnID || snapshot.ActiveTurn.SessionID != barrier.SourceSessionID ||
			snapshot.ActiveTurn.AttemptID != barrier.SourceAttemptID || int64(snapshot.ActiveTurn.Epoch) != barrier.ExecutionEpoch ||
			int64(snapshot.ActiveTurn.ControlRevision) != barrier.ControlRevision {
			return ClosureSettlement{}, fmt.Errorf("closure settlement source Turn does not match barrier: %w", ErrClosureSettlementFenceLost)
		}
		guard := workflow.TurnGuard{
			TurnID: snapshot.ActiveTurn.ID, SessionID: snapshot.ActiveTurn.SessionID,
			AttemptID: snapshot.ActiveTurn.AttemptID, Role: snapshot.ActiveTurn.Role,
			Epoch: snapshot.ActiveTurn.Epoch, ControlRevision: snapshot.ActiveTurn.ControlRevision,
			ChangeProposalID: snapshot.ActiveTurn.ChangeProposalID,
			ExpectedHeadSHA:  snapshot.ActiveTurn.ExpectedHeadSHA,
		}
		turn = &guard
	}
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedAt); err != nil {
		return ClosureSettlement{}, err
	}
	internalEventID, err := randomUUID()
	if err != nil {
		return ClosureSettlement{}, err
	}
	event := workflow.ClosureSettledEvent{EventMetadata: workflow.EventMetadata{
		ID: internalEventID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem,
		ExpectedRevision: snapshot.Revision,
	}, ClosureID: barrier.ClosureID, Turn: turn, AssignmentsExist: assignmentsExist}
	decision := workflow.Reduce(snapshot, event)
	if err := validateWorkflowDecision(snapshot, decision); err != nil || decision.Disposition != workflow.DispositionApplied || decision.Reason != workflow.ReasonClosureSettled {
		return ClosureSettlement{}, fmt.Errorf("closure settlement reducer decision is %s/%s (validation %v): %w", decision.Disposition, decision.Reason, err, ErrClosureSettlementFenceLost)
	}
	eventPayload, err := json.Marshal(map[string]any{
		"closure_id": barrier.ClosureID, "source_turn_id": barrier.SourceTurnID,
		"source_execution_epoch":  barrier.ExecutionEpoch,
		"source_control_revision": barrier.ControlRevision,
		"assignments_exist":       assignmentsExist,
	})
	if err != nil {
		return ClosureSettlement{}, err
	}
	if err := insertInternalEventTx(ctx, tx, internalEventID, job, workflow.EventKindClosureSettled, eventPayload, observedAt); err != nil {
		return ClosureSettlement{}, err
	}
	if barrier.SourceTurnID != "" {
		if err := settleClosedAgentTurnTx(ctx, tx, barrier); err != nil {
			return ClosureSettlement{}, err
		}
	}
	var settledAt time.Time
	if err := tx.QueryRow(ctx, `
UPDATE workflow_closure_barriers
SET settled_at = clock_timestamp(), internal_event_id = $3, applied_revision = $4
WHERE workflow_id = $1 AND closure_id = $2 AND settled_at IS NULL
RETURNING settled_at`, barrier.WorkflowID, barrier.ClosureID, internalEventID,
		int64(decision.Snapshot.Revision)).Scan(&settledAt); err != nil {
		return ClosureSettlement{}, fmt.Errorf("mark closure barrier settled: %w", ErrClosureSettlementFenceLost)
	}
	jobResult, err := json.Marshal(map[string]any{
		"closure_id": barrier.ClosureID, "workflow_revision": barrier.WorkflowRevision,
		"applied_revision": decision.Snapshot.Revision, "source_turn_id": barrier.SourceTurnID,
		"internal_event_id": internalEventID,
	})
	if err != nil {
		return ClosureSettlement{}, err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, jobResult, ErrClosureSettlementFenceLost); err != nil {
		return ClosureSettlement{}, err
	}
	if err := persistAppliedDecisionWithInternalProvenance(ctx, tx, "", "", internalEventID, barrier.WorkflowID, snapshot, decision, ""); err != nil {
		return ClosureSettlement{}, err
	}
	if err := completeInternalEventTx(ctx, tx, internalEventID, decision); err != nil {
		return ClosureSettlement{}, err
	}
	barrier.SettledAt = &settledAt
	barrier.CurrentWorkflowRevision = int64(decision.Snapshot.Revision)
	if err := tx.Commit(ctx); err != nil {
		return ClosureSettlement{}, fmt.Errorf("commit closure settlement completion: %w", err)
	}
	return barrier.settlement(), nil
}

func cancelPendingAgentTurnPreparationsTx(ctx context.Context, tx pgx.Tx, workflowID string) error {
	if _, err := tx.Exec(ctx, `
UPDATE job_attempts AS attempt
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE,
    last_error = 'Issue closure cancelled Agent Turn preparation'
FROM jobs AS job
WHERE job.workflow_id = $1 AND job.kind = 'PREPARE_AGENT_TURN'
  AND job.status = 'LEASED' AND job.id = attempt.job_id
  AND attempt.attempt_number = job.attempt_count
  AND attempt.lease_token = job.lease_token AND attempt.status = 'LEASED'`, workflowID); err != nil {
		return fmt.Errorf("fail cancelled Agent Turn preparation attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), updated_at = clock_timestamp(),
    last_error = 'Issue closure cancelled Agent Turn preparation'
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'
  AND status IN ('AVAILABLE', 'LEASED')`, workflowID); err != nil {
		return fmt.Errorf("cancel Agent Turn preparation: %w", err)
	}
	return nil
}

func resolveCompletedClosureSettlement(ctx context.Context, tx pgx.Tx, lease JobLease) (ClosureSettlement, bool, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 || strings.TrimSpace(lease.LeaseOwner) == "" {
		return ClosureSettlement{}, false, nil
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil || job.Kind != SettleClosureJobKind || job.Queue != WorkflowActionQueue ||
		job.Status != JobSucceeded || job.WorkflowID != lease.WorkflowID ||
		job.WorkflowAttemptID != lease.WorkflowAttemptID || job.AgentAssignmentID != lease.AgentAssignmentID ||
		job.AgentSessionID != lease.AgentSessionID || job.AgentTurnID != lease.AgentTurnID ||
		job.ExecutionEpoch != lease.ExecutionEpoch || job.AttemptCount != lease.Attempt ||
		!bytes.Equal(job.Payload, lease.Payload) {
		return ClosureSettlement{}, false, nil
	}
	var attemptStatus string
	if err := tx.QueryRow(ctx, `
SELECT status FROM job_attempts
WHERE job_id = $1 AND attempt_number = $2 AND lease_owner = $3 AND lease_token = $4`,
		lease.ID, lease.Attempt, lease.LeaseOwner, lease.LeaseToken).Scan(&attemptStatus); err != nil || attemptStatus != "SUCCEEDED" {
		return ClosureSettlement{}, false, nil
	}
	var barrier lockedClosureBarrier
	if err := tx.QueryRow(ctx, `
SELECT barrier.workflow_id::text, barrier.closure_id, barrier.workflow_revision,
       workflow.state_revision, COALESCE(barrier.source_turn_id::text, ''),
       COALESCE(barrier.source_session_id::text, ''), COALESCE(barrier.source_attempt_id::text, ''),
       COALESCE(barrier.source_execution_epoch, 0), COALESCE(barrier.source_control_revision, 0),
       barrier.runtime_stopped_at, barrier.settled_at,
       COALESCE(barrier.stop_job_id::text, ''), barrier.settlement_job_id::text
FROM workflow_closure_barriers AS barrier
JOIN workflows AS workflow ON workflow.id = barrier.workflow_id
JOIN workflow_internal_events AS event ON event.id = barrier.internal_event_id
WHERE barrier.settlement_job_id = $1 AND barrier.settled_at IS NOT NULL
  AND event.source_job_id = $1 AND event.applied_at IS NOT NULL`, lease.ID).Scan(
		&barrier.WorkflowID, &barrier.ClosureID, &barrier.WorkflowRevision,
		&barrier.CurrentWorkflowRevision, &barrier.SourceTurnID, &barrier.SourceSessionID,
		&barrier.SourceAttemptID, &barrier.ExecutionEpoch, &barrier.ControlRevision,
		&barrier.RuntimeStoppedAt, &barrier.SettledAt, &barrier.StopJobID,
		&barrier.SettlementJobID); err != nil {
		return ClosureSettlement{}, false, nil
	}
	return barrier.settlement(), true, nil
}

type lockedClosureBarrier struct {
	WorkflowID, ClosureID, SourceTurnID, SourceSessionID, SourceAttemptID string
	StopJobID, SettlementJobID                                            string
	WorkflowRevision, CurrentWorkflowRevision                             int64
	ExecutionEpoch, ControlRevision                                       int64
	RuntimeStoppedAt, SettledAt                                           *time.Time
}

func (barrier lockedClosureBarrier) settlement() ClosureSettlement {
	return ClosureSettlement{
		WorkflowID: barrier.WorkflowID, ClosureID: barrier.ClosureID,
		WorkflowRevision: barrier.WorkflowRevision, CurrentWorkflowRevision: barrier.CurrentWorkflowRevision,
		SourceTurnID:   barrier.SourceTurnID,
		ExecutionEpoch: barrier.ExecutionEpoch, ControlRevision: barrier.ControlRevision,
		RuntimeStoppedAt: barrier.RuntimeStoppedAt, SettledAt: barrier.SettledAt,
	}
}

func requireClosureRuntimeStopped(ctx context.Context, tx pgx.Tx, barrier lockedClosureBarrier) error {
	if barrier.SourceTurnID == "" || barrier.StopJobID == "" {
		return ErrClosureSettlementFenceLost
	}
	var stopStatus JobStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, barrier.StopJobID).Scan(&stopStatus); err != nil {
		return ErrClosureSettlementFenceLost
	}
	if barrier.RuntimeStoppedAt == nil || stopStatus != JobSucceeded {
		return ErrClosureSettlementUnsettled
	}
	return nil
}

func lockClosureWorkflow(ctx context.Context, tx pgx.Tx, lease JobLease) error {
	if !validUUID(lease.WorkflowID) {
		return ErrClosureSettlementFenceLost
	}
	var workflowID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM workflows WHERE id = $1 FOR UPDATE`, lease.WorkflowID).Scan(&workflowID); err != nil || workflowID != lease.WorkflowID {
		return ErrClosureSettlementFenceLost
	}
	return nil
}

func lockClosureJob(ctx context.Context, tx pgx.Tx, lease JobLease, expectedKind string) (Job, closureJobPayload, lockedClosureBarrier, error) {
	job, err := lockFencedWorkflowJob(ctx, tx, lease, expectedKind, ErrClosureSettlementFenceLost)
	if err != nil {
		return Job{}, closureJobPayload{}, lockedClosureBarrier{}, err
	}
	var payload closureJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.WorkflowID != job.WorkflowID ||
		payload.WorkflowAttemptID != job.WorkflowAttemptID || payload.WorkflowRevision <= 0 ||
		payload.ClosureID == "" || payload.TurnID != job.AgentTurnID || payload.SessionID != job.AgentSessionID ||
		payload.ExecutionEpoch != job.ExecutionEpoch {
		return Job{}, closureJobPayload{}, lockedClosureBarrier{}, ErrClosureSettlementFenceLost
	}
	var barrier lockedClosureBarrier
	var admissionClosedAt time.Time
	err = tx.QueryRow(ctx, `
SELECT workflow_id::text, closure_id, workflow_revision,
       COALESCE(source_turn_id::text, ''), COALESCE(source_session_id::text, ''),
       COALESCE(source_attempt_id::text, ''), COALESCE(source_execution_epoch, 0),
       COALESCE(source_control_revision, 0), mutation_admission_closed_at,
       runtime_stopped_at, settled_at, COALESCE(stop_job_id::text, ''), settlement_job_id::text
FROM workflow_closure_barriers
WHERE workflow_id = $1 AND closure_id = $2 FOR UPDATE`, payload.WorkflowID, payload.ClosureID).Scan(
		&barrier.WorkflowID, &barrier.ClosureID, &barrier.WorkflowRevision,
		&barrier.SourceTurnID, &barrier.SourceSessionID, &barrier.SourceAttemptID,
		&barrier.ExecutionEpoch, &barrier.ControlRevision, &admissionClosedAt,
		&barrier.RuntimeStoppedAt, &barrier.SettledAt, &barrier.StopJobID, &barrier.SettlementJobID,
	)
	if err != nil || barrier.SettledAt != nil || barrier.WorkflowRevision != payload.WorkflowRevision ||
		barrier.SourceTurnID != payload.TurnID || barrier.SourceSessionID != payload.SessionID ||
		barrier.ExecutionEpoch != payload.ExecutionEpoch ||
		barrier.ControlRevision != payload.ControlRevision ||
		expectedKind == StopAgentTurnJobKind && barrier.StopJobID != job.ID ||
		expectedKind == SettleClosureJobKind && barrier.SettlementJobID != job.ID {
		return Job{}, closureJobPayload{}, lockedClosureBarrier{}, ErrClosureSettlementFenceLost
	}
	if barrier.SourceTurnID != "" && barrier.SourceAttemptID != payload.WorkflowAttemptID {
		return Job{}, closureJobPayload{}, lockedClosureBarrier{}, ErrClosureSettlementFenceLost
	}
	var workflowRevision int64
	var closureID string
	if err := tx.QueryRow(ctx, `
SELECT state_revision, COALESCE(closure_id, '') FROM workflows
WHERE id = $1 AND status = 'CLOSING' FOR UPDATE`, payload.WorkflowID).Scan(&workflowRevision, &closureID); err != nil ||
		workflowRevision < payload.WorkflowRevision || closureID != payload.ClosureID {
		return Job{}, closureJobPayload{}, lockedClosureBarrier{}, ErrClosureSettlementFenceLost
	}
	barrier.CurrentWorkflowRevision = workflowRevision
	if barrier.SourceTurnID != "" {
		var workflowID, attemptID, assignmentID, sessionID, status string
		var epoch, turnControlRevision, sessionControlRevision int64
		var admissionOpen bool
		if err := tx.QueryRow(ctx, `
SELECT assignment.workflow_id::text, turn.workflow_attempt_id::text, assignment.id::text,
       session.id::text, turn.execution_epoch, turn.control_revision,
       session.control_revision, turn.status, turn.mutation_admission_open
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE turn.id = $1 FOR UPDATE`, barrier.SourceTurnID).Scan(
			&workflowID, &attemptID, &assignmentID, &sessionID, &epoch, &turnControlRevision,
			&sessionControlRevision, &status, &admissionOpen,
		); err != nil || workflowID != job.WorkflowID || attemptID != job.WorkflowAttemptID ||
			assignmentID != job.AgentAssignmentID || sessionID != job.AgentSessionID ||
			epoch != job.ExecutionEpoch || turnControlRevision != barrier.ControlRevision ||
			sessionControlRevision != barrier.ControlRevision || status != string(AgentTurnCancelling) || admissionOpen {
			return Job{}, closureJobPayload{}, lockedClosureBarrier{}, ErrClosureSettlementFenceLost
		}
	}
	return job, payload, barrier, nil
}

func settleClosedAgentTurnTx(ctx context.Context, tx pgx.Tx, barrier lockedClosureBarrier) error {
	var executionJob Job
	executionJob, err := scanJob(tx.QueryRow(ctx, jobSelect+`
 WHERE kind = $1 AND agent_turn_id = $2 AND execution_epoch = $3 FOR UPDATE`,
		RunAgentTurnJobKind, barrier.SourceTurnID, barrier.ExecutionEpoch))
	if err != nil {
		return fmt.Errorf("lock closed Agent Turn execution job: %w", err)
	}
	if executionJob.Status == JobLeased {
		result, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'FAILED', finished_at = clock_timestamp(), retryable = FALSE,
    last_error = 'Issue closure stopped Agent Turn'
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`,
			executionJob.ID, executionJob.AttemptCount, executionJob.LeaseToken)
		if err != nil || result.RowsAffected() != 1 {
			return ErrClosureSettlementFenceLost
		}
	} else if executionJob.Status != JobAvailable {
		return ErrClosureSettlementFenceLost
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_turn_slots WHERE agent_turn_id = $1`, barrier.SourceTurnID); err != nil {
		return fmt.Errorf("release closed Agent Turn slot: %w", err)
	}
	result, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'CANCELLED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), last_error = 'Issue closure stopped Agent Turn'
WHERE id = $1 AND status IN ('AVAILABLE', 'LEASED')`, executionJob.ID)
	if err != nil || result.RowsAffected() != 1 {
		return ErrClosureSettlementFenceLost
	}
	result, err = tx.Exec(ctx, `
UPDATE agent_turns
SET status = 'INTERRUPTED', active = FALSE, owner_id = NULL, owner_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    completed_at = clock_timestamp(), last_error = 'Issue closure stopped Agent Turn'
WHERE id = $1 AND agent_session_id = $2 AND workflow_attempt_id = $3
  AND execution_epoch = $4 AND control_revision = $5 AND status = 'CANCELLING'
  AND NOT mutation_admission_open AND active`, barrier.SourceTurnID, barrier.SourceSessionID,
		barrier.SourceAttemptID, barrier.ExecutionEpoch, barrier.ControlRevision)
	if err != nil || result.RowsAffected() != 1 {
		return ErrClosureSettlementFenceLost
	}
	return nil
}

func validateReconciledMutationOutcome(action string, outcome RecoveredMutationOutcome) (json.RawMessage, error) {
	switch outcome.State {
	case MutationSucceeded:
		result, err := canonicalJSON(outcome.Result)
		if err != nil {
			return nil, fmt.Errorf("%s: result: %w", action, err)
		}
		if outcome.LastError != "" {
			return nil, fmt.Errorf("%s: successful outcome has an error", action)
		}
		return result, nil
	case MutationFailed:
		if strings.TrimSpace(outcome.LastError) == "" || len(outcome.Result) != 0 {
			return nil, fmt.Errorf("%s: failed outcome requires only an error", action)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: outcome must be SUCCEEDED or FAILED", action)
	}
}

func lockFencedWorkflowJob(ctx context.Context, tx pgx.Tx, lease JobLease, expectedKind string, fenceErr error) (Job, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 || strings.TrimSpace(lease.LeaseOwner) == "" {
		return Job{}, fenceErr
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil || job.Kind != expectedKind || job.Queue != WorkflowActionQueue ||
		job.Status != JobLeased || job.LeaseOwner != lease.LeaseOwner ||
		job.LeaseToken != lease.LeaseToken || job.AttemptCount != lease.Attempt ||
		job.WorkflowID != lease.WorkflowID || job.WorkflowAttemptID != lease.WorkflowAttemptID ||
		job.AgentAssignmentID != lease.AgentAssignmentID || job.AgentSessionID != lease.AgentSessionID ||
		job.AgentTurnID != lease.AgentTurnID || job.ExecutionEpoch != lease.ExecutionEpoch ||
		!bytes.Equal(job.Payload, lease.Payload) {
		return Job{}, fenceErr
	}
	var jobLive, attemptLive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`, job.ID).Scan(&jobLive); err != nil || !jobLive {
		return Job{}, fenceErr
	}
	if err := tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_owner = $4 AND lease_token = $3
       AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		job.ID, lease.Attempt, lease.LeaseToken, lease.LeaseOwner).Scan(&attemptLive); err != nil || !attemptLive {
		return Job{}, fenceErr
	}
	return job, nil
}

func completeAcknowledgementJobTx(ctx context.Context, tx pgx.Tx, job Job, result json.RawMessage, fenceErr error) error {
	canonical, err := canonicalJSON(result)
	if err != nil {
		return err
	}
	attemptResult, err := tx.Exec(ctx, `
UPDATE job_attempts
SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, canonical)
	if err != nil || attemptResult.RowsAffected() != 1 {
		return fenceErr
	}
	jobResult, err := tx.Exec(ctx, `
UPDATE jobs
SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL, leased_at = NULL,
    lease_expires_at = NULL, heartbeat_at = NULL, updated_at = clock_timestamp(),
    completed_at = clock_timestamp(), result = $4, last_error = NULL
WHERE id = $1 AND lease_token = $2 AND attempt_count = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, canonical)
	if err != nil || jobResult.RowsAffected() != 1 {
		return fenceErr
	}
	return nil
}

func isFencedWorkflowJob(kind string) bool {
	return kind == ReconcilePendingEventsJobKind || kind == StopAgentTurnJobKind ||
		kind == SettleClosureJobKind || kind == PrepareAgentTurnJobKind ||
		kind == CollectAssignmentsJobKind || kind == "COMPLETE_ASSIGNMENTS" ||
		kind == "CANCEL_ASSIGNMENT_RETENTION" ||
		kind == ReconcileGitHubLabelsJobKind || kind == PublishHumanHandoffJobKind ||
		kind == EscalateWorkflowActionFailureJobKind
}
