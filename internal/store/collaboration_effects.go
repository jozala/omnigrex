package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
)

// collaborationMutationEffectState binds a positive internal receipt to its
// actual Workflow, originating Participant and tool. Equal content or original
// input ownership alone cannot prove which Participant applied an operation.
func (s *Store) collaborationMutationEffectState(ctx context.Context, workflowID, operation, participant, tool string) (collaboration.State, error) {
	var state collaboration.State
	if !validUUID(workflowID) || !validUUID(operation) || !validUUID(participant) {
		return state, pgx.ErrNoRows
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT c.state FROM work_item_collaboration c
 JOIN agent_turns t ON t.workflow_id=c.workflow_id
 JOIN agent_sessions session ON session.id=t.agent_session_id
 JOIN tool_invocations i ON i.agent_turn_id=t.id
 WHERE c.workflow_id=$1 AND i.id=$2 AND session.agent_participant_id=$3
 AND i.tool_name=$4 AND i.kind='MUTATION' AND i.external_service='omnigrex'
 AND i.external_resource_id=c.workflow_id::text`, workflowID, operation, participant, tool).Scan(&raw)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

// HasClarificationResolutionEffect proves this exact operation's interpretation
// using the same explicit evidence scope as Resolve, not original ownership.
func (s *Store) HasClarificationResolutionEffect(ctx context.Context, workflowID, operation, participant, roundID string, resolutions []collaboration.Resolution) (bool, error) {
	state, err := s.collaborationMutationEffectState(ctx, workflowID, operation, participant, "resolve_clarification")
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, round := range state.Rounds {
		if round.ID == roundID && state.RoundInScope(round, participant) {
			return round.HasResolutionEffect(operation, resolutions), nil
		}
	}
	return false, nil
}
