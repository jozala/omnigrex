package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentTurnRuntimeDisposition is the database-authoritative relationship between an Agent Turn and a Runtime Process.
type AgentTurnRuntimeDisposition string

const (
	AgentTurnRuntimeActive   AgentTurnRuntimeDisposition = "ACTIVE"
	AgentTurnRuntimeLive     AgentTurnRuntimeDisposition = "LIVE"
	AgentTurnRuntimeExpired  AgentTurnRuntimeDisposition = "EXPIRED"
	AgentTurnRuntimeRecovery AgentTurnRuntimeDisposition = "RECOVERY"
	AgentTurnRuntimeTerminal AgentTurnRuntimeDisposition = "TERMINAL"
)

// AgentTurnRuntimeIdentity is the complete non-secret identity carried by a managed Runtime Process.
type AgentTurnRuntimeIdentity struct {
	AssignmentID          string
	AgentSessionID        string
	AgentTurnID           string
	ExecutionEpoch        int64
	RuntimeProfileName    string
	RuntimeProfileVersion string
}

// AgentTurnRuntimeState classifies one durable Agent Turn and its exact Runtime Process identity.
type AgentTurnRuntimeState struct {
	Identity    AgentTurnRuntimeIdentity
	Disposition AgentTurnRuntimeDisposition
}

// ClaimAndRecoverExpiredAgentTurn atomically fences one expired execution selected with SKIP LOCKED.
// A false result means this caller observed no currently claimable expired execution.
func (store *Store) ClaimAndRecoverExpiredAgentTurn(ctx context.Context) (AgentTurnRecovery, bool, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, false, fmt.Errorf("begin claimed Agent Turn recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var jobID string
	err = tx.QueryRow(ctx, `
SELECT job.id::text
FROM jobs AS job
JOIN agent_turns AS turn
  ON turn.id = job.agent_turn_id AND turn.execution_epoch = job.execution_epoch
JOIN job_attempts AS attempt
  ON attempt.job_id = job.id AND attempt.attempt_number = job.attempt_count
LEFT JOIN agent_turn_slots AS slot ON slot.agent_turn_id = turn.id
WHERE job.kind = 'RUN_AGENT_TURN' AND job.status = 'LEASED'
  AND job.lease_expires_at <= clock_timestamp()
  AND attempt.status = 'LEASED' AND attempt.lease_token = job.lease_token
  AND attempt.lease_expires_at <= clock_timestamp()
  AND turn.active AND turn.owner_id IS NOT NULL AND turn.owner_token IS NOT NULL
  AND turn.lease_expires_at <= clock_timestamp()
  AND turn.recovery_started_at IS NULL
  AND (slot.agent_turn_id IS NULL OR slot.lease_expires_at <= clock_timestamp())
ORDER BY turn.lease_expires_at, turn.id
FOR UPDATE OF job SKIP LOCKED
LIMIT 1`).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnRecovery{}, false, fmt.Errorf("commit empty claimed Agent Turn recovery: %w", err)
		}
		return AgentTurnRecovery{}, false, nil
	}
	if err != nil {
		return AgentTurnRecovery{}, false, fmt.Errorf("claim expired Agent Turn: %w", err)
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID))
	if err != nil {
		return AgentTurnRecovery{}, false, fmt.Errorf("read claimed expired Agent Turn job: %w", err)
	}
	recovery, err := recoverExpiredAgentTurnTx(ctx, tx, job)
	if err != nil {
		return AgentTurnRecovery{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, false, fmt.Errorf("commit claimed Agent Turn recovery: %w", err)
	}
	return recovery, true, nil
}

// HasRecoverableExpiredAgentTurn reports whether an execution is currently eligible for recovery without fencing it.
func (store *Store) HasRecoverableExpiredAgentTurn(ctx context.Context) (bool, error) {
	var recoverable bool
	err := store.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM jobs AS job
    JOIN agent_turns AS turn
      ON turn.id = job.agent_turn_id AND turn.execution_epoch = job.execution_epoch
    JOIN job_attempts AS attempt
      ON attempt.job_id = job.id AND attempt.attempt_number = job.attempt_count
    LEFT JOIN agent_turn_slots AS slot ON slot.agent_turn_id = turn.id
    WHERE job.kind = 'RUN_AGENT_TURN' AND job.status = 'LEASED'
      AND job.lease_expires_at <= clock_timestamp()
      AND attempt.status = 'LEASED' AND attempt.lease_token = job.lease_token
      AND attempt.lease_expires_at <= clock_timestamp()
      AND turn.active AND turn.owner_id IS NOT NULL AND turn.owner_token IS NOT NULL
      AND turn.lease_expires_at <= clock_timestamp()
      AND turn.recovery_started_at IS NULL
      AND (slot.agent_turn_id IS NULL OR slot.lease_expires_at <= clock_timestamp())
)`).Scan(&recoverable)
	if err != nil {
		return false, fmt.Errorf("inspect recoverable expired Agent Turns: %w", err)
	}
	return recoverable, nil
}

// ClassifyAgentTurnRuntime classifies an exact discovered Runtime Process identity.
// The boolean is false when no durable Agent Turn has that complete identity.
func (store *Store) ClassifyAgentTurnRuntime(ctx context.Context, identity AgentTurnRuntimeIdentity) (AgentTurnRuntimeState, bool, error) {
	if err := validateAgentTurnRuntimeIdentity(identity); err != nil {
		return AgentTurnRuntimeState{}, false, err
	}
	state, found, err := classifyAgentTurnRuntime(ctx, store.pool, identity)
	if err != nil {
		return AgentTurnRuntimeState{}, false, fmt.Errorf("classify Agent Turn Runtime Process: %w", err)
	}
	return state, found, nil
}

// FenceDuplicateAgentTurnRuntime atomically revokes a live exact identity and enters its existing recovery path.
// Repeating the call after an ambiguous commit returns the original recovery barrier.
func (store *Store) FenceDuplicateAgentTurnRuntime(ctx context.Context, identity AgentTurnRuntimeIdentity) (AgentTurnRecovery, error) {
	if err := validateAgentTurnRuntimeIdentity(identity); err != nil {
		return AgentTurnRecovery{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("begin duplicate Agent Turn Runtime Process fence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := scanJob(tx.QueryRow(ctx, jobSelect+`
WHERE kind = 'RUN_AGENT_TURN' AND agent_turn_id = $1 AND execution_epoch = $2
FOR UPDATE`, identity.AgentTurnID, identity.ExecutionEpoch))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock duplicate Agent Turn execution job: %w", err)
	}
	if job.AgentAssignmentID != identity.AssignmentID || job.AgentSessionID != identity.AgentSessionID {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if job.Status == JobLeased {
		var attemptExists bool
		err = tx.QueryRow(ctx, `
SELECT TRUE FROM job_attempts
WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3
FOR UPDATE`, job.ID, job.AttemptCount, job.LeaseToken).Scan(&attemptExists)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
		if err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("lock duplicate Agent Turn execution attempt: %w", err)
		}
	}
	if _, err := lockHierarchy(ctx, tx, hierarchyFromJob(job), job.WorkflowAttemptID, false); err != nil {
		return AgentTurnRecovery{}, err
	}
	turn, err := lockAgentTurn(ctx, tx, identity.AgentTurnID)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if turn.AgentSessionID != identity.AgentSessionID || turn.ExecutionEpoch != identity.ExecutionEpoch {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}

	if job.Status != JobLeased {
		_, found, err := classifyAgentTurnRuntime(ctx, tx, identity)
		if err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("classify repeated duplicate Agent Turn Runtime Process fence: %w", err)
		}
		if !found {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
		recovery, err := readAgentTurnRecovery(ctx, tx, identity.AgentTurnID, identity.ExecutionEpoch)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && recovery.JobID != job.ID {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
		if err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("read repeated duplicate Agent Turn Runtime Process recovery: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("commit repeated duplicate Agent Turn Runtime Process fence: %w", err)
		}
		return recovery, nil
	}

	if err := lockAgentTurnSlots(ctx, tx); err != nil {
		return AgentTurnRecovery{}, err
	}
	var slotExists bool
	err = tx.QueryRow(ctx, `SELECT TRUE FROM agent_turn_slots WHERE agent_turn_id = $1 FOR UPDATE`, identity.AgentTurnID).Scan(&slotExists)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("lock duplicate Agent Turn slot: %w", err)
	}
	state, found, err := classifyAgentTurnRuntime(ctx, tx, identity)
	if err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("classify locked duplicate Agent Turn Runtime Process: %w", err)
	}
	if !found || state.Disposition != AgentTurnRuntimeLive {
		return AgentTurnRecovery{}, ErrAgentTurnFenceLost
	}

	var revokedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() - interval '1 microsecond'`).Scan(&revokedAt); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("calculate duplicate Agent Turn lease revocation: %w", err)
	}
	updates := []struct {
		name  string
		query string
		args  []any
	}{
		{"execution job", `UPDATE jobs SET lease_expires_at = $2 WHERE id = $1 AND status = 'LEASED'`, []any{job.ID, revokedAt}},
		{"execution attempt", `UPDATE job_attempts SET lease_expires_at = $4 WHERE job_id = $1 AND attempt_number = $2 AND lease_token = $3 AND status = 'LEASED'`, []any{job.ID, job.AttemptCount, job.LeaseToken, revokedAt}},
		{"Agent Turn", `UPDATE agent_turns SET lease_expires_at = $3 WHERE id = $1 AND execution_epoch = $2 AND active`, []any{identity.AgentTurnID, identity.ExecutionEpoch, revokedAt}},
		{"Agent Turn slot", `UPDATE agent_turn_slots SET lease_expires_at = $3 WHERE agent_turn_id = $1 AND execution_epoch = $2`, []any{identity.AgentTurnID, identity.ExecutionEpoch, revokedAt}},
	}
	for _, update := range updates {
		result, err := tx.Exec(ctx, update.query, update.args...)
		if err != nil {
			return AgentTurnRecovery{}, fmt.Errorf("revoke duplicate %s lease: %w", update.name, err)
		}
		if result.RowsAffected() != 1 {
			return AgentTurnRecovery{}, ErrAgentTurnFenceLost
		}
	}
	recovery, err := recoverExpiredAgentTurnTx(ctx, tx, job)
	if err != nil {
		return AgentTurnRecovery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentTurnRecovery{}, fmt.Errorf("commit duplicate Agent Turn Runtime Process fence: %w", err)
	}
	return recovery, nil
}

type agentTurnRuntimeStateQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func classifyAgentTurnRuntime(ctx context.Context, queryer agentTurnRuntimeStateQueryer, identity AgentTurnRuntimeIdentity) (AgentTurnRuntimeState, bool, error) {
	state, err := scanAgentTurnRuntimeState(queryer.QueryRow(ctx, agentTurnRuntimeStateSelect+`
WHERE turn.id = $1 AND turn.execution_epoch = $2
  AND session.id = $3 AND assignment.id = $4
  AND session.runtime_profile_name = $5 AND session.runtime_profile_version = $6`,
		identity.AgentTurnID, identity.ExecutionEpoch, identity.AgentSessionID, identity.AssignmentID,
		identity.RuntimeProfileName, identity.RuntimeProfileVersion,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnRuntimeState{}, false, nil
	}
	return state, err == nil, err
}

// ListAgentTurnRuntimeStates returns a deterministic page of durable Runtime Process identities.
func (store *Store) ListAgentTurnRuntimeStates(ctx context.Context, afterTurnID string, limit int) ([]AgentTurnRuntimeState, error) {
	if afterTurnID != "" && !validUUID(afterTurnID) {
		return nil, errors.New("list Agent Turn Runtime Process states: invalid cursor")
	}
	if limit <= 0 || limit > 10_000 {
		return nil, errors.New("list Agent Turn Runtime Process states: limit must be between 1 and 10000")
	}
	rows, err := store.pool.Query(ctx, agentTurnRuntimeStateSelect+`
WHERE ($1 = '' OR turn.id > NULLIF($1, '')::uuid)
ORDER BY turn.id
LIMIT $2`, afterTurnID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Agent Turn Runtime Process states: %w", err)
	}
	defer rows.Close()
	states := make([]AgentTurnRuntimeState, 0, limit)
	for rows.Next() {
		state, err := scanAgentTurnRuntimeState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Agent Turn Runtime Process state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Agent Turn Runtime Process states: %w", err)
	}
	return states, nil
}

func validateAgentTurnRuntimeIdentity(identity AgentTurnRuntimeIdentity) error {
	if !validUUID(identity.AssignmentID) || !validUUID(identity.AgentSessionID) || !validUUID(identity.AgentTurnID) ||
		identity.ExecutionEpoch <= 0 || strings.TrimSpace(identity.RuntimeProfileName) == "" ||
		strings.TrimSpace(identity.RuntimeProfileVersion) == "" {
		return errors.New("Agent Turn Runtime Process identity is invalid")
	}
	return nil
}

const agentTurnRuntimeStateSelect = `
WITH observation AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT assignment.id::text, session.id::text, turn.id::text, turn.execution_epoch,
       session.runtime_profile_name, session.runtime_profile_version,
       CASE
           WHEN turn.recovery_started_at IS NOT NULL AND turn.recovery_settled_at IS NULL THEN 'RECOVERY'
           WHEN NOT turn.active THEN 'TERMINAL'
           WHEN execution.status IN ('SUCCEEDED', 'FAILED', 'CANCELLED') THEN 'TERMINAL'
           WHEN authority.matches
            AND execution.lease_expires_at <= observation.observed_at
            AND attempt.lease_expires_at <= observation.observed_at
            AND turn.lease_expires_at <= observation.observed_at
            AND slot.lease_expires_at <= observation.observed_at THEN 'EXPIRED'
           WHEN authority.matches
            AND execution.lease_expires_at > observation.observed_at
            AND attempt.lease_expires_at > observation.observed_at
            AND turn.lease_expires_at > observation.observed_at
            AND slot.lease_expires_at > observation.observed_at THEN 'LIVE'
           ELSE 'ACTIVE'
       END
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS assignment
  ON assignment.id = session.agent_assignment_id
 AND assignment.runtime_profile_name = session.runtime_profile_name
 AND assignment.runtime_profile_version = session.runtime_profile_version
LEFT JOIN jobs AS execution
  ON execution.agent_turn_id = turn.id AND execution.execution_epoch = turn.execution_epoch
 AND execution.kind = 'RUN_AGENT_TURN'
LEFT JOIN job_attempts AS attempt
  ON attempt.job_id = execution.id AND attempt.attempt_number = execution.attempt_count
LEFT JOIN agent_turn_slots AS slot ON slot.agent_turn_id = turn.id
CROSS JOIN observation
CROSS JOIN LATERAL (
    SELECT turn.active
       AND turn.status IN ('RUNNING', 'CANCELLING', 'SETTLING', 'RECONCILING')
       AND assignment.status = 'ACTIVE'
       AND session.status IN ('CREATING', 'ACTIVE')
       AND session.control_owner = 'AUTOMATION'
       AND session.control_revision = turn.control_revision
       AND execution.id IS NOT NULL
       AND execution.queue = 'agent-turns'
       AND execution.status = 'LEASED'
       AND execution.max_attempts = 1
       AND execution.attempt_count > 0
       AND execution.workflow_id = turn.workflow_id
       AND execution.workflow_attempt_id = turn.workflow_attempt_id
       AND execution.agent_assignment_id = assignment.id
       AND execution.agent_session_id = session.id
       AND execution.payload = jsonb_build_object(
           'agent_turn_id', turn.id,
           'agent_session_id', session.id,
           'execution_epoch', turn.execution_epoch,
           'control_revision', turn.control_revision
       )
       AND attempt.job_id IS NOT NULL
       AND attempt.status = 'LEASED'
       AND attempt.lease_owner = execution.lease_owner
       AND attempt.lease_token = execution.lease_token
       AND turn.owner_id IS NOT NULL
       AND turn.owner_token IS NOT NULL
       AND slot.agent_turn_id IS NOT NULL
       AND slot.agent_session_id = session.id
       AND slot.execution_epoch = turn.execution_epoch
       AND slot.control_revision = turn.control_revision
       AND slot.owner_id = turn.owner_id
       AND slot.owner_token = turn.owner_token AS matches
) AS authority
`

func scanAgentTurnRuntimeState(row rowScanner) (AgentTurnRuntimeState, error) {
	var state AgentTurnRuntimeState
	err := row.Scan(
		&state.Identity.AssignmentID, &state.Identity.AgentSessionID, &state.Identity.AgentTurnID,
		&state.Identity.ExecutionEpoch, &state.Identity.RuntimeProfileName,
		&state.Identity.RuntimeProfileVersion, &state.Disposition,
	)
	return state, err
}
