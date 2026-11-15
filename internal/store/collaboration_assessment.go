package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
)

func (s *Store) AssessHumanInput(ctx context.Context, lease AgentTurnLease, operation string, assessments []collaboration.Assessment) error {
	return s.withLockedAgentTurnLease(ctx, lease, "assess human input", func(tx pgx.Tx, turn lockedTurn) error {
		if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
			return ErrMutationAdmissionClosed
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT request FROM tool_invocations WHERE id=$1 AND agent_turn_id=$2 AND execution_epoch=$3 AND tool_name='assess_human_input' AND state='IN_FLIGHT' AND external_service='omnigrex' AND external_resource_id=$4`, operation, turn.ID, turn.ExecutionEpoch, lease.JobLease.WorkflowID).Scan(&raw); err != nil {
			return err
		}
		var intent struct {
			Assessments []collaboration.Assessment `json:"assessments"`
		}
		if json.Unmarshal(raw, &intent) != nil || !slices.EqualFunc(intent.Assessments, assessments, collaboration.Assessment.Equal) {
			return ErrMutationOperationConflict
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if state.HasAssessmentEffect(operation, assessments) {
			return nil
		}
		if err = state.Assess(operation, turn.AgentParticipantID, turn.ID, assessments); err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
}

func (s *Store) HasHumanInputAssessmentEffect(ctx context.Context, workflowID, operation, participant string, assessments []collaboration.Assessment) (bool, error) {
	state, err := s.collaborationMutationEffectState(ctx, workflowID, operation, participant, "assess_human_input")
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, a := range assessments {
		if !slices.ContainsFunc(state.Inputs, func(in collaboration.Input) bool {
			return in.ID == a.InputID && state.InputDeliveredInScope(in, participant)
		}) {
			return false, nil
		}
	}
	return state.HasAssessmentEffect(operation, assessments), nil
}
