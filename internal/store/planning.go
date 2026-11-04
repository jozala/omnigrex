package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
)

func (s *Store) ReservePlanningCheckpoint(ctx context.Context, lease AgentTurnLease, operation string, c collaboration.PlanningContent) (collaboration.PlanningRevision, error) {
	var r collaboration.PlanningRevision
	err := s.withLockedAgentTurnLease(ctx, lease, "reserve planning checkpoint", func(tx pgx.Tx, turn lockedTurn) error {
		if err := planningInvocation(ctx, tx, turn, operation, "publish_plan_checkpoint"); err != nil {
			return err
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if state.Planning != nil {
			for _, prior := range state.Planning.Revisions {
				if prior.Published || prior.PublicationFailed {
					continue
				}
				var failed bool
				if err = tx.QueryRow(ctx, `SELECT state='FAILED' FROM tool_invocations WHERE id=$1`, strings.TrimPrefix(prior.ID, "P")).Scan(&failed); err != nil {
					return err
				}
				if failed {
					if err = state.FailCheckpoint(prior.ID); err != nil {
						return err
					}
				}
			}
		}
		r, err = state.ReserveCheckpoint("P"+operation, turn.AgentParticipantID, turn.ID, c)
		if err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
	return r, err
}
func planningInvocation(ctx context.Context, tx pgx.Tx, turn lockedTurn, operation, tool string) error {
	if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
		return ErrMutationAdmissionClosed
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_invocations i JOIN agent_participants p ON p.id=$4 WHERE i.id=$1 AND i.agent_turn_id=$2 AND i.execution_epoch=$3 AND i.tool_name=$5 AND i.state='IN_FLIGHT' AND p.role='PLANNER')`, operation, turn.ID, turn.ExecutionEpoch, turn.AgentParticipantID, tool).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return collaboration.ErrInvalid
	}
	return nil
}
func (s *Store) ConfirmPlanningPublication(ctx context.Context, workflowID, id string, source collaboration.Source) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = rehydrateWorkflow(ctx, tx, workflowID); err != nil {
		return err
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	if err = state.ConfirmCheckpoint(id, source); err != nil {
		return err
	}
	if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) GetPlanningCheckpoint(ctx context.Context, workflowID, id string) (collaboration.PlanningRevision, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw); err != nil {
		return collaboration.PlanningRevision{}, err
	}
	var state collaboration.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return collaboration.PlanningRevision{}, err
	}
	if state.Planning != nil {
		for _, r := range state.Planning.Revisions {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return collaboration.PlanningRevision{}, collaboration.ErrInvalid
}
func (s *Store) CompletePlanning(ctx context.Context, lease AgentTurnLease, operation, revision, commit, evidence string, resolved bool) error {
	return s.withLockedAgentTurnLease(ctx, lease, "complete planning", func(tx pgx.Tx, turn lockedTurn) error {
		if err := planningInvocation(ctx, tx, turn, operation, "complete_planning"); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='refresh_default_branch' AND state='SUCCEEDED' ORDER BY invocation_number DESC LIMIT 1`, turn.ID, turn.ExecutionEpoch).Scan(&raw); err != nil {
			return err
		}
		var snapshot struct {
			Status string `json:"status"`
			SHA    string `json:"sha"`
		}
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Status != "current" || snapshot.SHA != commit {
			return collaboration.ErrInvalid
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if err = state.ValidatePlanning(operation, revision, turn.AgentParticipantID, turn.ID, commit, evidence, resolved); err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
}
func (s *Store) GetPlanningValidation(ctx context.Context, workflowID, turnID string) (collaboration.PlanRevalidation, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT entry FROM work_item_collaboration,LATERAL jsonb_array_elements(state->'planning'->'revalidations') entry WHERE workflow_id=$1 AND entry->>'turn_id'=$2`, workflowID, turnID).Scan(&raw)
	if err != nil {
		return collaboration.PlanRevalidation{}, err
	}
	var v collaboration.PlanRevalidation
	err = json.Unmarshal(raw, &v)
	return v, err
}
func (s *Store) IsAwaitingPlanConfirmation(ctx context.Context, workflowID, participant string) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state collaboration.State
	if err = json.Unmarshal(raw, &state); err != nil {
		return false, err
	}
	return state.AwaitingPlanConfirmation(participant), nil
}

type PlanningActivationState struct {
	Exists, PlanningOnly bool
	State                string
}

func (s *Store) GetPlanningActivationState(ctx context.Context, repositoryID, issueID, issueNumber int64) (PlanningActivationState, error) {
	var state PlanningActivationState
	err := s.pool.QueryRow(ctx, `SELECT planning_only,status FROM workflows WHERE repository_id=$1 AND issue_id=$2 AND issue_number=$3`, repositoryID, issueID, issueNumber).Scan(&state.PlanningOnly, &state.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Exists = true
	return state, nil
}
