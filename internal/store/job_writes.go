package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

type jobInsert struct {
	queue          string
	kind           string
	payload        json.RawMessage
	priority       int
	maxAttempts    int
	idempotencyKey string
	scope          jobInsertScope
	provenance     jobInsertProvenance
}

type jobInsertScope struct {
	workflowID        string
	workflowAttemptID string
	agentAssignmentID string
	agentSessionID    string
	agentTurnID       string
	executionEpoch    int64
}

type jobInsertProvenance struct {
	normalizedEventID       string
	agentTurnSettlementID   string
	workflowInternalEventID string
	actionKey               string
}

const jobInsertSQL = `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id, agent_assignment_id,
    agent_session_id, agent_turn_id, execution_epoch, normalized_event_id,
    agent_turn_settlement_id, workflow_internal_event_id, action_key
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', $5, clock_timestamp(), $6,
        $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

func insertJobTx(ctx context.Context, tx pgx.Tx, job jobInsert) (string, error) {
	job, err := validateJobInsert(job)
	if err != nil {
		return "", err
	}
	id, err := randomUUID()
	if err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, jobInsertSQL+` RETURNING id::text`, jobInsertArgs(id, job)...).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func insertIdempotentJobTx(ctx context.Context, tx pgx.Tx, job jobInsert) (string, error) {
	job, err := validateJobInsert(job)
	if err != nil {
		return "", err
	}
	id, err := randomUUID()
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, jobInsertSQL+` ON CONFLICT DO NOTHING RETURNING id::text`, jobInsertArgs(id, job)...).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	var matching bool
	err = tx.QueryRow(ctx, `
SELECT id::text,
       queue = $2
       AND kind = $3
       AND payload = $4::jsonb
       AND priority = $5
       AND max_attempts = $6
       AND enqueue_delay_microseconds = 0
       AND workflow_id IS NOT DISTINCT FROM $7::uuid
       AND workflow_attempt_id IS NOT DISTINCT FROM $8::uuid
       AND agent_assignment_id IS NOT DISTINCT FROM $9::uuid
       AND agent_session_id IS NOT DISTINCT FROM $10::uuid
       AND agent_turn_id IS NOT DISTINCT FROM $11::uuid
       AND execution_epoch IS NOT DISTINCT FROM $12::bigint
       AND normalized_event_id IS NOT DISTINCT FROM $13::uuid
       AND agent_turn_settlement_id IS NOT DISTINCT FROM $14::uuid
       AND workflow_internal_event_id IS NOT DISTINCT FROM $15::uuid
       AND action_key IS NOT DISTINCT FROM $16::text
       AND retention_generation_id IS NULL
FROM jobs
WHERE idempotency_key = $1`, job.idempotencyKey, job.queue, job.kind, job.payload,
		job.priority, job.maxAttempts, nullableString(job.scope.workflowID),
		nullableString(job.scope.workflowAttemptID), nullableString(job.scope.agentAssignmentID),
		nullableString(job.scope.agentSessionID), nullableString(job.scope.agentTurnID),
		nullableEpoch(job.scope.executionEpoch), nullableString(job.provenance.normalizedEventID),
		nullableString(job.provenance.agentTurnSettlementID), nullableString(job.provenance.workflowInternalEventID),
		nullableString(job.provenance.actionKey)).Scan(&id, &matching)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !matching {
		return "", ErrJobIdempotencyConflict
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func validateJobInsert(job jobInsert) (jobInsert, error) {
	if strings.TrimSpace(job.queue) == "" || strings.TrimSpace(job.kind) == "" {
		return jobInsert{}, errors.New("insert job: queue and kind are required")
	}
	if strings.TrimSpace(job.idempotencyKey) == "" {
		return jobInsert{}, errors.New("insert job: idempotency key is required")
	}
	if job.maxAttempts <= 0 {
		return jobInsert{}, errors.New("insert job: max attempts must be positive")
	}
	if !json.Valid(job.payload) {
		return jobInsert{}, errors.New("insert job payload: not valid JSON")
	}
	ids := []string{
		job.scope.workflowID, job.scope.workflowAttemptID, job.scope.agentAssignmentID,
		job.scope.agentSessionID, job.scope.agentTurnID, job.provenance.normalizedEventID,
		job.provenance.agentTurnSettlementID, job.provenance.workflowInternalEventID,
	}
	for _, id := range ids {
		if id != "" && !validUUID(id) {
			return jobInsert{}, errors.New("insert job: identity is not a UUID")
		}
	}
	if job.scope.workflowID == "" ||
		job.scope.workflowAttemptID != "" && job.scope.workflowID == "" ||
		job.scope.agentAssignmentID != "" && job.scope.workflowID == "" ||
		job.scope.agentSessionID != "" && job.scope.agentAssignmentID == "" ||
		job.scope.agentTurnID != "" && job.scope.agentSessionID == "" ||
		(job.scope.agentTurnID == "") != (job.scope.executionEpoch == 0) {
		return jobInsert{}, errors.New("insert job: scope hierarchy is incomplete")
	}
	provenanceCount := 0
	for _, id := range []string{
		job.provenance.normalizedEventID,
		job.provenance.agentTurnSettlementID,
		job.provenance.workflowInternalEventID,
	} {
		if id != "" {
			provenanceCount++
		}
	}
	if provenanceCount > 1 || provenanceCount == 0 && job.provenance.actionKey != "" ||
		provenanceCount == 1 && strings.TrimSpace(job.provenance.actionKey) == "" {
		return jobInsert{}, errors.New("insert job: provenance is inconsistent")
	}
	return job, nil
}

func jobInsertArgs(id string, job jobInsert) []any {
	return []any{
		id, job.queue, job.kind, job.payload, job.priority, job.maxAttempts, job.idempotencyKey,
		job.scope.workflowID, nullableString(job.scope.workflowAttemptID), nullableString(job.scope.agentAssignmentID),
		nullableString(job.scope.agentSessionID), nullableString(job.scope.agentTurnID), nullableEpoch(job.scope.executionEpoch),
		nullableString(job.provenance.normalizedEventID), nullableString(job.provenance.agentTurnSettlementID),
		nullableString(job.provenance.workflowInternalEventID), nullableString(job.provenance.actionKey),
	}
}
