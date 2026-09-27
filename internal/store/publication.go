package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrAgentTurnPublicationConflict = errors.New("Agent Turn in-progress publication conflict")

// ListParticipantPublicationMutations reads prior, terminal publication
// operations from the same Participant, independent of Agent Session or Attempt.
func (store *Store) ListParticipantPublicationMutations(ctx context.Context, lease AgentTurnLease) ([]MutationReservation, error) {
	var mutations []MutationReservation
	err := store.withLockedAgentTurnLeaseStatus(ctx, lease, "read Participant publication mutations", true, func(tx pgx.Tx, turn lockedTurn) error {
		rows, err := tx.Query(ctx, `
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
  AND mutation.tool_name IN ('publish_changes', 'open_pr')
  AND mutation.state = 'SUCCEEDED'
ORDER BY source_turn.created_at, mutation.invocation_number`,
			lease.JobLease.WorkflowID, lease.AgentAssignmentID, lease.ID, turn.CreatedAt)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			mutation, err := scanMutation(rows)
			if err != nil {
				return err
			}
			mutations = append(mutations, mutation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read Participant publication mutations: %w", err)
	}
	return mutations, nil
}

// BindAgentTurnPublication records a corroborated publication under the live
// Turn fence. A second, differing binding can never replace it.
func (store *Store) BindAgentTurnPublication(ctx context.Context, lease AgentTurnLease, publication AgentTurnPublication) error {
	hasPR := publication.SourceOpenPRMutationID != ""
	if !validUUID(publication.SourcePublishMutationID) || publication.HeadRef == "" || publication.HeadSHA == "" || publication.BaseRef == "" ||
		(hasPR && (!validUUID(publication.SourceOpenPRMutationID) || publication.PullRequestID <= 0 ||
			publication.PullRequestNumber <= 0 || publication.PullRequestNodeID == "")) ||
		(!hasPR && (publication.PullRequestID != 0 || publication.PullRequestNumber != 0 || publication.PullRequestNodeID != "")) {
		return ErrAgentTurnPublicationConflict
	}
	return store.withLockedAgentTurnLeaseStatus(ctx, lease, "bind in-progress publication", true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning || turn.ChangeProposalID != "" {
			return ErrAgentTurnPublicationConflict
		}
		ids := []string{publication.SourcePublishMutationID}
		if publication.SourceOpenPRMutationID != "" {
			ids = append(ids, publication.SourceOpenPRMutationID)
		}
		var count int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM tool_invocations AS mutation
JOIN agent_turns AS source ON source.id = mutation.agent_turn_id
JOIN agent_sessions AS source_session ON source_session.id = source.agent_session_id
WHERE mutation.id = ANY($1::uuid[]) AND mutation.kind = 'MUTATION'
  AND mutation.state = 'SUCCEEDED' AND source.workflow_id = $2
  AND source_session.agent_assignment_id = $3 AND source.id <> $4
  AND source.created_at <= $5 AND source.completed_at IS NOT NULL
  AND (mutation.id = $6 AND mutation.tool_name = 'publish_changes'
       OR mutation.id = $7 AND mutation.tool_name = 'open_pr')`,
			ids, lease.JobLease.WorkflowID,
			lease.AgentAssignmentID, lease.ID, turn.CreatedAt, publication.SourcePublishMutationID,
			nullableString(publication.SourceOpenPRMutationID)).Scan(&count); err != nil {
			return err
		}
		wanted := 1
		if publication.SourceOpenPRMutationID != "" {
			wanted++
		}
		if count != wanted {
			return ErrAgentTurnPublicationConflict
		}
		var id string
		err := tx.QueryRow(ctx, `
INSERT INTO agent_turn_publications (
    agent_turn_id, execution_epoch, head_ref, head_sha, base_ref,
    pull_request_id, pull_request_number, pull_request_node_id,
    source_publish_mutation_id, source_open_pr_mutation_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (agent_turn_id) DO UPDATE SET agent_turn_id = EXCLUDED.agent_turn_id
WHERE agent_turn_publications.execution_epoch = EXCLUDED.execution_epoch
  AND agent_turn_publications.head_ref = EXCLUDED.head_ref
  AND agent_turn_publications.head_sha = EXCLUDED.head_sha
  AND agent_turn_publications.base_ref = EXCLUDED.base_ref
  AND agent_turn_publications.pull_request_id IS NOT DISTINCT FROM EXCLUDED.pull_request_id
  AND agent_turn_publications.pull_request_number IS NOT DISTINCT FROM EXCLUDED.pull_request_number
  AND agent_turn_publications.pull_request_node_id IS NOT DISTINCT FROM EXCLUDED.pull_request_node_id
  AND agent_turn_publications.source_publish_mutation_id = EXCLUDED.source_publish_mutation_id
  AND agent_turn_publications.source_open_pr_mutation_id IS NOT DISTINCT FROM EXCLUDED.source_open_pr_mutation_id
RETURNING agent_turn_id::text`, lease.ID, lease.ExecutionEpoch, publication.HeadRef, publication.HeadSHA,
			publication.BaseRef, nullablePublicationInt64(publication.PullRequestID), nullablePublicationInt64(publication.PullRequestNumber),
			nullableString(publication.PullRequestNodeID), publication.SourcePublishMutationID,
			nullableString(publication.SourceOpenPRMutationID)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentTurnPublicationConflict
		}
		return err
	})
}

func nullablePublicationInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
