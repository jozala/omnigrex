package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/workflow"
)

const VerifyTerminalIntentJobKind = "VERIFY_TERMINAL_INTENT"
const RevalidateTerminalIntentJobKind = "REVALIDATE_TERMINAL_INTENT"

var ErrTerminalCorroborationConflict = errors.New("terminal intent corroboration checkpoint conflicts with recorded evidence")
var ErrTerminalCorroborationHeadMoved = errors.New("Change Proposal head moved during terminal corroboration")

// TerminalCorroboration is the durable, credential-free identity of an eligible
// terminal mutation awaiting fresh observations after its Runtime Process stops.
type TerminalCorroboration struct {
	TurnID             string
	ExecutionEpoch     int64
	WorkflowID         string
	WorkflowAttemptID  string
	SourceInvocationID string
	VerificationJobID  string
	ExecutionJobID     string
	PromptOutcome      json.RawMessage
	PromptError        string
	PendingSince       time.Time
	LastFailureCode    string
}

// TerminalCorroborationContext is read-only evidence for a leased verifier.
// It does not grant mutation or settlement authority.
type TerminalCorroborationContext struct {
	Checkpoint                TerminalCorroboration
	Execution                 AgentTurnExecutionContext
	Mutations                 []MutationReservation
	PriorPublicationMutations []MutationReservation
	WorkflowRevision          int64
}

func (store *Store) GetTerminalCorroborationContext(ctx context.Context, lease JobLease) (TerminalCorroborationContext, error) {
	reactivation := lease.Kind == RevalidateTerminalIntentJobKind
	if lease.Kind != VerifyTerminalIntentJobKind && !reactivation || lease.Queue != "agent-turn-recovery" ||
		!validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TerminalCorroborationContext{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workflowStateForLock string
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`,
		lease.WorkflowID).Scan(&workflowStateForLock); err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	var jobLive, attemptLive bool
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $2 AND attempt_count = $3
       AND lease_expires_at > clock_timestamp()
FROM jobs WHERE id = $1 AND kind = $4 AND queue = 'agent-turn-recovery'
FOR UPDATE`, lease.ID, lease.LeaseToken, lease.Attempt, lease.Kind).Scan(&jobLive)
	if err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if !jobLive {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if job.Kind != lease.Kind || job.Queue != "agent-turn-recovery" ||
		job.Status != JobLeased || job.LeaseOwner != lease.LeaseOwner || job.LeaseToken != lease.LeaseToken ||
		job.AttemptCount != lease.Attempt || job.WorkflowID != lease.WorkflowID ||
		job.WorkflowAttemptID != lease.WorkflowAttemptID || job.AgentAssignmentID != lease.AgentAssignmentID ||
		job.AgentSessionID != lease.AgentSessionID || job.AgentTurnID != lease.AgentTurnID ||
		job.ExecutionEpoch != lease.ExecutionEpoch {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	err = tx.QueryRow(ctx, `
SELECT status = 'LEASED' AND lease_token = $3 AND lease_owner = $4
       AND lease_expires_at > clock_timestamp()
FROM job_attempts WHERE job_id = $1 AND attempt_number = $2 FOR UPDATE`,
		lease.ID, lease.Attempt, lease.LeaseToken, lease.LeaseOwner).Scan(&attemptLive)
	if err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if !attemptLive {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	var result TerminalCorroborationContext
	checkpoint := &result.Checkpoint
	checkpointQuery := `
SELECT agent_turn_id::text, execution_epoch, workflow_id::text, workflow_attempt_id::text,
       source_invocation_id::text, verification_job_id::text, execution_job_id::text, prompt_outcome,
       COALESCE(prompt_error, ''), pending_since, COALESCE(last_failure_code, '')
FROM agent_turn_corroborations
WHERE verification_job_id = $1 AND state = 'PENDING'`
	if reactivation {
		checkpointQuery = `
SELECT agent_turn_id::text, execution_epoch, workflow_id::text, workflow_attempt_id::text,
       source_invocation_id::text, verification_job_id::text, execution_job_id::text, prompt_outcome,
       COALESCE(prompt_error, ''), pending_since, COALESCE(last_failure_code, '')
FROM agent_turn_corroborations
WHERE agent_turn_id = $1 AND state = 'HANDED_OFF'`
	}
	checkpointID := lease.ID
	if reactivation {
		checkpointID = lease.AgentTurnID
	}
	err = tx.QueryRow(ctx, checkpointQuery, checkpointID).Scan(
		&checkpoint.TurnID, &checkpoint.ExecutionEpoch, &checkpoint.WorkflowID,
		&checkpoint.WorkflowAttemptID, &checkpoint.SourceInvocationID,
		&checkpoint.VerificationJobID, &checkpoint.ExecutionJobID, &checkpoint.PromptOutcome, &checkpoint.PromptError,
		&checkpoint.PendingSince, &checkpoint.LastFailureCode)
	if err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if checkpoint.TurnID != lease.AgentTurnID || checkpoint.ExecutionEpoch != lease.ExecutionEpoch ||
		checkpoint.WorkflowID != lease.WorkflowID || !reactivation && checkpoint.WorkflowAttemptID != lease.WorkflowAttemptID {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	if reactivation {
		var payload struct {
			SourceTurnID string `json:"source_turn_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.SourceTurnID != checkpoint.TurnID ||
			checkpoint.WorkflowAttemptID == lease.WorkflowAttemptID {
			return TerminalCorroborationContext{}, ErrJobLeaseLost
		}
		checkpoint.PendingSince = job.CreatedAt
		var latestFailure string
		err := tx.QueryRow(ctx, `
SELECT last_error FROM job_attempts
WHERE job_id = $1 AND attempt_number < $2 AND status = 'FAILED'
ORDER BY attempt_number DESC LIMIT 1`, job.ID, job.AttemptCount).Scan(&latestFailure)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return TerminalCorroborationContext{}, err
		}
		if err == nil {
			if !validCorroborationFailureCode(latestFailure) {
				return TerminalCorroborationContext{}, ErrTerminalCorroborationConflict
			}
			checkpoint.LastFailureCode = latestFailure
		}
	}
	turn, err := lockAgentTurn(ctx, tx, checkpoint.TurnID)
	if err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if (!reactivation && (!turn.active || turn.Status != AgentTurnCorroborating) ||
		reactivation && (turn.active || turn.Status != AgentTurnFailed)) || turn.MutationAdmissionOpen ||
		turn.WorkflowAttemptID != checkpoint.WorkflowAttemptID || turn.ExecutionEpoch != checkpoint.ExecutionEpoch ||
		turn.AgentSessionID != lease.AgentSessionID {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	turn.AgentAssignmentID = lease.AgentAssignmentID
	turn.AgentParticipantID = lease.AgentAssignmentID
	var workflowState string
	var activeAttempt bool
	if err := tx.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, attempt.active
FROM workflows AS workflow JOIN workflow_attempts AS attempt
  ON attempt.workflow_id = workflow.id
WHERE workflow.id = $1 AND attempt.id = $2`, checkpoint.WorkflowID, lease.WorkflowAttemptID).Scan(
		&workflowState, &result.WorkflowRevision, &activeAttempt); err != nil {
		return TerminalCorroborationContext{}, corroborationReadError(err)
	}
	if !activeAttempt || workflowState != "DEVELOPING" && workflowState != "REVIEWING" {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	if reactivation {
		snapshot, err := rehydrateWorkflow(ctx, tx, checkpoint.WorkflowID)
		if err != nil {
			return TerminalCorroborationContext{}, err
		}
		if !store.reducer.DefinitionCompatible(snapshot) {
			return TerminalCorroborationContext{}, ErrAgentTurnSettlementRejected
		}
		var activeTurn, stageMatches bool
		if err := tx.QueryRow(ctx, `
SELECT NOT EXISTS (SELECT 1 FROM agent_turns WHERE workflow_id = $1 AND active),
       EXISTS (SELECT 1 FROM workflow_attempts WHERE id = $2 AND current_stage = $3)`,
			checkpoint.WorkflowID, lease.WorkflowAttemptID, turn.Stage).Scan(&activeTurn, &stageMatches); err != nil {
			return TerminalCorroborationContext{}, err
		}
		if !activeTurn || !stageMatches {
			return TerminalCorroborationContext{}, ErrJobLeaseLost
		}
	}
	var execution AgentTurnExecutionContext
	if err := tx.QueryRow(ctx, `
SELECT id::text, repository_id, repository_owner, repository_name, issue_id, issue_number
FROM workflows WHERE id = $1`, checkpoint.WorkflowID).Scan(
		&execution.WorkflowID, &execution.Repository.ID, &execution.Repository.Owner,
		&execution.Repository.Name, &execution.Issue.ID, &execution.Issue.Number); err != nil {
		return TerminalCorroborationContext{}, err
	}
	participant, err := scanAgentAssignment(tx.QueryRow(ctx, agentAssignmentSelect+` WHERE id = $1`, lease.AgentAssignmentID))
	if err != nil {
		return TerminalCorroborationContext{}, err
	}
	session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, lease.AgentSessionID))
	if err != nil {
		return TerminalCorroborationContext{}, err
	}
	if participant.WorkflowID != checkpoint.WorkflowID || session.AgentAssignmentID != participant.ID ||
		session.ControlOwner != SessionControlAutomation {
		return TerminalCorroborationContext{}, ErrJobLeaseLost
	}
	execution.Participant, execution.Assignment, execution.Session, execution.Turn = participant, participant, session, turn.AgentTurn
	if turn.ChangeProposalID != "" {
		proposal := &AgentTurnChangeProposal{}
		if err := tx.QueryRow(ctx, `
SELECT id::text, pull_request_id, pull_request_number, base_ref, base_sha, head_ref, head_sha
FROM change_proposals WHERE id = $1 AND workflow_id = $2 AND active`,
			turn.ChangeProposalID, checkpoint.WorkflowID).Scan(
			&proposal.ID, &proposal.PullRequestID, &proposal.PullRequestNumber,
			&proposal.BaseRef, &proposal.BaseSHA, &proposal.HeadRef, &proposal.HeadSHA); err != nil {
			return TerminalCorroborationContext{}, err
		}
		execution.ChangeProposal = proposal
	} else {
		publication := &AgentTurnPublication{}
		err := tx.QueryRow(ctx, `
SELECT head_ref, head_sha, base_ref, COALESCE(pull_request_id, 0), COALESCE(pull_request_number, 0),
       COALESCE(pull_request_node_id, ''), source_publish_mutation_id::text,
       COALESCE(source_open_pr_mutation_id::text, '')
FROM agent_turn_publications WHERE agent_turn_id = $1 AND execution_epoch = $2`,
			checkpoint.TurnID, checkpoint.ExecutionEpoch).Scan(
			&publication.HeadRef, &publication.HeadSHA, &publication.BaseRef,
			&publication.PullRequestID, &publication.PullRequestNumber, &publication.PullRequestNodeID,
			&publication.SourcePublishMutationID, &publication.SourceOpenPRMutationID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return TerminalCorroborationContext{}, err
		}
		if err == nil {
			execution.Publication = publication
		}
	}
	result.Execution = execution
	rows, err := tx.Query(ctx, mutationLedgerSelect, checkpoint.TurnID, checkpoint.ExecutionEpoch)
	if err != nil {
		return TerminalCorroborationContext{}, err
	}
	for rows.Next() {
		mutation, err := scanMutation(rows)
		if err != nil {
			rows.Close()
			return TerminalCorroborationContext{}, err
		}
		if mutation.State != MutationSucceeded && mutation.State != MutationFailed {
			rows.Close()
			return TerminalCorroborationContext{}, ErrAgentTurnMutationsUnsettled
		}
		result.Mutations = append(result.Mutations, mutation)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return TerminalCorroborationContext{}, err
	}
	if execution.Publication != nil && execution.Publication.PullRequestID > 0 {
		result.PriorPublicationMutations = make([]MutationReservation, 0)
		priorRows, err := tx.Query(ctx, `
SELECT mutation.id::text, mutation.agent_turn_id::text, mutation.execution_epoch, mutation.invocation_number,
       COALESCE(mutation.operation_id, ''), mutation.tool_name, mutation.request, mutation.state,
       COALESCE(mutation.external_service, ''), COALESCE(mutation.external_resource_id, ''), COALESCE(mutation.expected_sha, ''),
       mutation.result, COALESCE(mutation.last_error, ''), mutation.admitted_at, mutation.started_at, mutation.finished_at
FROM tool_invocations AS mutation
JOIN agent_turns AS source_turn ON source_turn.id = mutation.agent_turn_id
JOIN agent_sessions AS source_session ON source_session.id = source_turn.agent_session_id
WHERE source_turn.workflow_id = $1 AND source_session.agent_assignment_id = $2
  AND source_turn.id <> $3 AND source_turn.created_at <= $4
  AND source_turn.completed_at IS NOT NULL AND mutation.kind = 'MUTATION'
  AND mutation.tool_name IN ('publish_changes', 'open_pr') AND mutation.state = 'SUCCEEDED'
ORDER BY source_turn.created_at, mutation.invocation_number`,
			checkpoint.WorkflowID, lease.AgentAssignmentID, checkpoint.TurnID, turn.CreatedAt)
		if err != nil {
			return TerminalCorroborationContext{}, err
		}
		for priorRows.Next() {
			mutation, err := scanMutation(priorRows)
			if err != nil {
				priorRows.Close()
				return TerminalCorroborationContext{}, err
			}
			result.PriorPublicationMutations = append(result.PriorPublicationMutations, mutation)
		}
		err = priorRows.Err()
		priorRows.Close()
		if err != nil {
			return TerminalCorroborationContext{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TerminalCorroborationContext{}, err
	}
	return result, nil
}

func corroborationReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrAgentTurnFenceLost) {
		return ErrJobLeaseLost
	}
	return err
}

// BeginTerminalCorroboration atomically transfers a stopped, drained live
// Turn's authority from its execution job/slot to a durable verification job.
// The caller must never invoke this before Runtime Process cleanup succeeds.
func (store *Store) BeginTerminalCorroboration(ctx context.Context, lease AgentTurnLease, sourceInvocationID string, promptOutcome json.RawMessage, promptError, failureCode string) (TerminalCorroboration, error) {
	if !validUUID(sourceInvocationID) || !validSettlementLease(lease) || !validCorroborationPrompt(promptOutcome, promptError) ||
		!validCorroborationFailureCode(failureCode) {
		return TerminalCorroboration{}, ErrTerminalCorroborationConflict
	}
	if promptError == "" {
		// Persist only the allowlisted ACP stop reason, never caller-supplied text.
		promptOutcome = json.RawMessage(`{"stop_reason":"end_turn"}`)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TerminalCorroboration{}, fmt.Errorf("begin terminal corroboration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"terminal-corroboration:"+lease.ID+":"+fmt.Sprint(lease.ExecutionEpoch)); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("serialize terminal corroboration: %w", err)
	}
	var existing TerminalCorroboration
	err = tx.QueryRow(ctx, `
SELECT agent_turn_id::text, execution_epoch, workflow_id::text, workflow_attempt_id::text,
       source_invocation_id::text, verification_job_id::text, execution_job_id::text, prompt_outcome,
       COALESCE(prompt_error, ''), pending_since, COALESCE(last_failure_code, '')
FROM agent_turn_corroborations WHERE agent_turn_id = $1 FOR UPDATE`, lease.ID).Scan(
		&existing.TurnID, &existing.ExecutionEpoch, &existing.WorkflowID, &existing.WorkflowAttemptID,
		&existing.SourceInvocationID, &existing.VerificationJobID, &existing.ExecutionJobID, &existing.PromptOutcome,
		&existing.PromptError, &existing.PendingSince, &existing.LastFailureCode,
	)
	if err == nil {
		if existing.ExecutionEpoch != lease.ExecutionEpoch || existing.WorkflowID != lease.JobLease.WorkflowID ||
			existing.WorkflowAttemptID != lease.WorkflowAttemptID || existing.SourceInvocationID != sourceInvocationID ||
			existing.PromptError != promptError ||
			!sameCorroborationPrompt(existing.PromptOutcome, promptOutcome) {
			return TerminalCorroboration{}, ErrTerminalCorroborationConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TerminalCorroboration{}, fmt.Errorf("commit repeated terminal corroboration: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TerminalCorroboration{}, fmt.Errorf("read terminal corroboration checkpoint: %w", err)
	}
	job, turn, role, _, err := lockLiveAgentTurnSettlement(ctx, tx, lease)
	if err != nil {
		return TerminalCorroboration{}, err
	}
	if turn.Status != AgentTurnSettling {
		return TerminalCorroboration{}, ErrAgentTurnFenceLost
	}
	var successfulIntents int
	var sourceTool string
	rows, err := tx.Query(ctx, mutationLedgerSelect, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		return TerminalCorroboration{}, err
	}
	for rows.Next() {
		mutation, err := scanMutation(rows)
		if err != nil {
			rows.Close()
			return TerminalCorroboration{}, err
		}
		if mutation.State != MutationSucceeded {
			continue
		}
		switch mutation.ToolName {
		case "request_review", "submit_review", "report_blocked", "confirm_prior_terminal_intent":
			successfulIntents++
			if mutation.ID == sourceInvocationID {
				sourceTool = mutation.ToolName
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return TerminalCorroboration{}, err
	}
	if successfulIntents != 1 ||
		role == workflow.RoleDeveloper && sourceTool != "request_review" && sourceTool != "confirm_prior_terminal_intent" ||
		role == workflow.RoleReviewer && sourceTool != "submit_review" && sourceTool != "confirm_prior_terminal_intent" ||
		role != workflow.RoleDeveloper && role != workflow.RoleReviewer {
		return TerminalCorroboration{}, ErrTerminalCorroborationConflict
	}
	if sourceTool == "confirm_prior_terminal_intent" {
		var source struct {
			SourceTool         string `json:"source_tool"`
			SourceInvocationID string `json:"source_invocation_id"`
		}
		var sourceResult []byte
		if err := tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE id = $1`, sourceInvocationID).Scan(&sourceResult); err != nil ||
			json.Unmarshal(sourceResult, &source) != nil {
			return TerminalCorroboration{}, ErrTerminalCorroborationConflict
		}
		if source.SourceInvocationID == "" || role == workflow.RoleDeveloper && source.SourceTool != "request_review" ||
			role == workflow.RoleReviewer && source.SourceTool != "submit_review" {
			return TerminalCorroboration{}, ErrTerminalCorroborationConflict
		}
	}
	verificationJobID, err := randomUUID()
	if err != nil {
		return TerminalCorroboration{}, err
	}
	checkpoint := TerminalCorroboration{
		TurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, WorkflowID: job.WorkflowID,
		WorkflowAttemptID: turn.WorkflowAttemptID, SourceInvocationID: sourceInvocationID,
		VerificationJobID: verificationJobID, ExecutionJobID: job.ID, PromptOutcome: promptOutcome,
		PromptError: promptError, LastFailureCode: failureCode,
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO jobs (id, queue, kind, payload, status, priority, max_attempts,
                  idempotency_key, workflow_id, workflow_attempt_id,
                  agent_assignment_id, agent_session_id, agent_turn_id, execution_epoch)
VALUES ($1, 'agent-turn-recovery', $2, '{}'::jsonb, 'AVAILABLE', 80, 1000000,
        $3, $4, $5, $6, $7, $8, $9)
RETURNING id::text`, verificationJobID, VerifyTerminalIntentJobKind,
		"terminal-corroboration:"+turn.ID+":"+fmt.Sprint(turn.ExecutionEpoch),
		job.WorkflowID, turn.WorkflowAttemptID, job.AgentAssignmentID,
		turn.AgentSessionID, turn.ID, turn.ExecutionEpoch).Scan(&checkpoint.VerificationJobID); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("enqueue terminal corroboration: %w", err)
	}
	ownerHash := sha256.Sum256([]byte(lease.OwnerToken))
	if err := tx.QueryRow(ctx, `
INSERT INTO agent_turn_corroborations (agent_turn_id, execution_epoch, workflow_id,
    workflow_attempt_id, source_invocation_id, execution_job_id,
    execution_owner_id, execution_owner_token_sha256, prompt_outcome, prompt_error,
    state, last_failure_code, verification_job_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'PENDING', $11, $12)
RETURNING pending_since`, turn.ID, turn.ExecutionEpoch, job.WorkflowID,
		turn.WorkflowAttemptID, sourceInvocationID, job.ID, lease.OwnerID,
		ownerHash[:], nullableJSON(promptOutcome), nullableString(promptError),
		failureCode, verificationJobID).Scan(&checkpoint.PendingSince); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("persist terminal corroboration checkpoint: %w", err)
	}
	const result = `{"pending_terminal_corroboration":true}`
	completedAttempt, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'SUCCEEDED', finished_at = clock_timestamp(), result = $4
WHERE job_id = $1 AND lease_token = $2 AND attempt_number = $3 AND status = 'LEASED'`,
		job.ID, job.LeaseToken, job.AttemptCount, result)
	if err != nil || completedAttempt.RowsAffected() != 1 {
		return TerminalCorroboration{}, ErrAgentTurnFenceLost
	}
	completedJob, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'SUCCEEDED', lease_owner = NULL, lease_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
    updated_at = clock_timestamp(), completed_at = clock_timestamp(), result = $3
WHERE id = $1 AND lease_token = $2 AND status = 'LEASED'`, job.ID, job.LeaseToken, result)
	if err != nil || completedJob.RowsAffected() != 1 {
		return TerminalCorroboration{}, ErrAgentTurnFenceLost
	}
	released, err := tx.Exec(ctx, `DELETE FROM agent_turn_slots
WHERE agent_turn_id = $1 AND owner_token = $2`, turn.ID, lease.OwnerToken)
	if err != nil || released.RowsAffected() != 1 {
		return TerminalCorroboration{}, ErrAgentTurnFenceLost
	}
	updated, err := tx.Exec(ctx, `
UPDATE agent_turns SET status = 'CORROBORATING', owner_id = NULL, owner_token = NULL,
    leased_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL
WHERE id = $1 AND execution_epoch = $2 AND status = 'SETTLING'
  AND owner_token = $3 AND active AND NOT mutation_admission_open`,
		turn.ID, turn.ExecutionEpoch, lease.OwnerToken)
	if err != nil || updated.RowsAffected() != 1 {
		return TerminalCorroboration{}, ErrAgentTurnFenceLost
	}
	if err := tx.Commit(ctx); err != nil {
		return TerminalCorroboration{}, fmt.Errorf("commit terminal corroboration checkpoint: %w", err)
	}
	return checkpoint, nil
}

func validCorroborationPrompt(outcome json.RawMessage, promptError string) bool {
	if promptError == "FAILURE" || promptError == "DEADLINE" {
		return len(outcome) == 0
	}
	if promptError != "" {
		return false
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(outcome, &response) != nil || len(response) != 1 {
		return false
	}
	var stopReason string
	return json.Unmarshal(response["stop_reason"], &stopReason) == nil && stopReason == "end_turn"
}

func sameCorroborationPrompt(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	leftJSON, leftErr := json.Marshal(a)
	rightJSON, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func validCorroborationFailureCode(code string) bool {
	switch code {
	case "deadline_exceeded", "invalid_api_response", "rate_limited",
		"permission_denied", "github_http_error", "transient_transport",
		"invalid_configuration", "github_dependency_unknown", "github_prerequisite_unavailable",
		"database_observation_unavailable", "workspace_observation_unavailable",
		"terminal_evidence_conflict", "head_changed_during_corroboration",
		"review_not_visible_yet":
		return true
	default:
		return false
	}
}

// GetConfirmablePriorTerminalIntent reads a successful ancestor terminal
// mutation under the current live Turn fence. It cannot submit an external
// effect or project that result into the new Turn's ledger by itself.
func (store *Store) GetConfirmablePriorTerminalIntent(ctx context.Context, lease AgentTurnLease, sourceID string) (MutationReservation, error) {
	if !validUUID(sourceID) {
		return MutationReservation{}, ErrMutationOperationConflict
	}
	var source MutationReservation
	err := store.withLockedAgentTurnLease(ctx, lease, "read confirmable prior terminal intent", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning || !turn.MutationAdmissionOpen {
			return ErrMutationAdmissionClosed
		}
		var sourceTurnID, sourceLineage, sourceSessionID, sourceAttemptID, sourceStatus string
		var sourceCompleted, sourceSettled bool
		if err := tx.QueryRow(ctx, `
SELECT source.id::text, source.operation_lineage_id::text, source.agent_session_id::text,
       source.workflow_attempt_id::text, source.status, source.completed_at IS NOT NULL,
       EXISTS (SELECT 1 FROM agent_turn_settlements AS settlement
               WHERE settlement.agent_turn_id = source.id
                 AND settlement.workflow_outcome = 'INFRASTRUCTURE_FAILED')
FROM agent_turns AS source
JOIN tool_invocations AS mutation ON mutation.agent_turn_id = source.id
WHERE mutation.id = $1 AND mutation.kind = 'MUTATION' AND mutation.state = 'SUCCEEDED'
  AND source.workflow_id = $2
  AND (source.outcome IS NULL OR source.outcome->>'stop_reason' = 'end_turn')
  AND (source.last_error IS NULL OR (source.last_error NOT LIKE 'ACP prompt was cancelled%'
       AND source.last_error NOT LIKE 'ACP prompt returned an invalid stop reason%'))`, sourceID, lease.JobLease.WorkflowID).Scan(
			&sourceTurnID, &sourceLineage, &sourceSessionID, &sourceAttemptID,
			&sourceStatus, &sourceCompleted, &sourceSettled); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMutationOperationConflict
			}
			return err
		}
		if sourceLineage != turn.operationLineageID || sourceSessionID != turn.AgentSessionID ||
			sourceAttemptID != turn.WorkflowAttemptID || !sourceCompleted || !sourceSettled ||
			(sourceStatus != string(AgentTurnFailed) && sourceStatus != string(AgentTurnInterrupted) && sourceStatus != string(AgentTurnTimedOut)) {
			return ErrMutationOperationConflict
		}
		var successfulTerminals int
		if err := tx.QueryRow(ctx, `
WITH RECURSIVE ancestors (id) AS (
    SELECT retry_of_turn_id FROM agent_turns WHERE id = $1
    UNION ALL
    SELECT source.retry_of_turn_id FROM agent_turns AS source
    JOIN ancestors ON source.id = ancestors.id WHERE ancestors.id IS NOT NULL
)
SELECT count(DISTINCT CASE WHEN terminal.tool_name = 'confirm_prior_terminal_intent'
            THEN COALESCE(terminal.result->>'source_invocation_id', terminal.id::text)
            ELSE terminal.id::text END) FROM ancestors AS prior_turn
JOIN tool_invocations AS terminal ON terminal.agent_turn_id = prior_turn.id
WHERE terminal.kind = 'MUTATION' AND terminal.state = 'SUCCEEDED'
  AND terminal.tool_name IN
    ('request_review', 'submit_review', 'report_blocked', 'confirm_prior_terminal_intent')`,
			turn.ID).Scan(&successfulTerminals); err != nil {
			return err
		}
		if successfulTerminals != 1 {
			return ErrMutationOperationConflict
		}
		if err := validateMutationReplayAncestor(ctx, tx, turn.ID, sourceTurnID); err != nil {
			return err
		}
		var err error
		source, err = getMutationByID(ctx, tx, sourceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMutationOperationConflict
			}
			return err
		}
		if source.State != MutationSucceeded ||
			(source.ToolName != "request_review" && source.ToolName != "submit_review") {
			return ErrMutationOperationConflict
		}
		return nil
	})
	return source, err
}

// HasPriorSuccessfulReviewForTurn prevents a retry from submitting a second
// native GitHub review for an already successful review mutation at this head.
// A cancelled prior prompt still leaves that external review in GitHub.
func (store *Store) HasPriorSuccessfulReviewForTurn(ctx context.Context, lease AgentTurnLease) (bool, error) {
	var exists bool
	err := store.withLockedAgentTurnLease(ctx, lease, "inspect prior submitted review", func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning || !turn.MutationAdmissionOpen {
			return ErrMutationAdmissionClosed
		}
		if turn.ExpectedHeadSHA == "" {
			return nil
		}
		return tx.QueryRow(ctx, `
WITH RECURSIVE prior_turns (id) AS (
    SELECT id FROM agent_turns WHERE id = $1
    UNION ALL
    SELECT source.retry_of_turn_id FROM agent_turns AS source
    JOIN prior_turns ON source.id = prior_turns.id WHERE source.retry_of_turn_id IS NOT NULL
)
SELECT EXISTS (
    SELECT 1 FROM prior_turns AS prior_turn
    JOIN agent_turns AS source ON source.id = prior_turn.id
    JOIN tool_invocations AS mutation ON mutation.agent_turn_id = source.id
    WHERE source.workflow_attempt_id = $2 AND source.agent_session_id = $3
      AND source.operation_lineage_id = $4
      AND mutation.kind = 'MUTATION' AND mutation.state = 'SUCCEEDED'
      AND mutation.tool_name = 'submit_review' AND mutation.expected_sha = $5
)`, turn.ID, turn.WorkflowAttemptID, turn.AgentSessionID,
			turn.operationLineageID, turn.ExpectedHeadSHA).Scan(&exists)
	})
	return exists, err
}

// RetryTerminalCorroboration releases only the exact verifier lease and
// schedules another read; no agent mutation or Workflow transition occurs.
func (store *Store) RetryTerminalCorroboration(ctx context.Context, lease JobLease, failureCode string, delay time.Duration) error {
	reactivation := lease.Kind == RevalidateTerminalIntentJobKind
	if lease.Kind != VerifyTerminalIntentJobKind && !reactivation || !validUUID(lease.ID) ||
		!validUUID(lease.LeaseToken) || lease.Attempt <= 0 ||
		!validCorroborationFailureCode(failureCode) || delay < 0 || delay > 365*24*time.Hour {
		return ErrJobLeaseLost
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workflowStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1 FOR UPDATE`,
		lease.WorkflowID).Scan(&workflowStatus); err != nil {
		return corroborationReadError(err)
	}
	if workflowStatus != "DEVELOPING" && workflowStatus != "REVIEWING" {
		return ErrJobLeaseLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, lease.ID))
	if err != nil {
		return corroborationReadError(err)
	}
	if job.Kind != lease.Kind || job.Queue != "agent-turn-recovery" ||
		job.Status != JobLeased || job.LeaseOwner != lease.LeaseOwner || job.LeaseToken != lease.LeaseToken ||
		job.AttemptCount != lease.Attempt || job.WorkflowID != lease.WorkflowID ||
		job.WorkflowAttemptID != lease.WorkflowAttemptID || job.AgentAssignmentID != lease.AgentAssignmentID ||
		job.AgentSessionID != lease.AgentSessionID || job.AgentTurnID != lease.AgentTurnID ||
		job.ExecutionEpoch != lease.ExecutionEpoch {
		return ErrJobLeaseLost
	}
	var jobLive, checkpointPending bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at > clock_timestamp() FROM jobs WHERE id = $1`,
		lease.ID).Scan(&jobLive); err != nil {
		return corroborationReadError(err)
	}
	if !jobLive {
		return ErrJobLeaseLost
	}
	checkpointQuery := `
SELECT state = 'PENDING' FROM agent_turn_corroborations
WHERE verification_job_id = $1 AND agent_turn_id = $2 AND execution_epoch = $3 FOR UPDATE`
	if reactivation {
		checkpointQuery = `SELECT state = 'HANDED_OFF' FROM agent_turn_corroborations
WHERE agent_turn_id = $1 AND execution_epoch = $2 FOR UPDATE`
	}
	checkpointArgs := []any{lease.ID, lease.AgentTurnID, lease.ExecutionEpoch}
	if reactivation {
		checkpointArgs = []any{lease.AgentTurnID, lease.ExecutionEpoch}
	}
	if err := tx.QueryRow(ctx, checkpointQuery, checkpointArgs...).Scan(&checkpointPending); err != nil {
		return corroborationReadError(err)
	}
	if !checkpointPending {
		return ErrJobLeaseLost
	}
	attempt, err := tx.Exec(ctx, `
UPDATE job_attempts SET status = 'FAILED', finished_at = clock_timestamp(),
    retryable = TRUE, last_error = $4
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3
  AND status = 'LEASED' AND lease_expires_at > clock_timestamp()`,
		lease.ID, lease.Attempt, lease.LeaseToken, failureCode)
	if err != nil {
		return err
	}
	if attempt.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	updated, err := tx.Exec(ctx, `
UPDATE jobs SET status = 'AVAILABLE', available_at = clock_timestamp() + $4 * interval '1 microsecond',
    lease_owner = NULL, lease_token = NULL, leased_at = NULL, lease_expires_at = NULL,
    heartbeat_at = NULL, updated_at = clock_timestamp(), last_error = $5
WHERE id = $1 AND attempt_count = $2 AND lease_token = $3 AND status = 'LEASED'`,
		lease.ID, lease.Attempt, lease.LeaseToken, delay.Microseconds(), failureCode)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrJobLeaseLost
	}
	if !reactivation {
		if _, err := tx.Exec(ctx, `
UPDATE agent_turn_corroborations SET last_failure_code = $2
WHERE verification_job_id = $1 AND state = 'PENDING'`, lease.ID, failureCode); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
