package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
)

// RequestReplanning only records exact operation evidence. Settlement owns
// dispatch after mutation drain, source shutdown and required evidence capture.
func (s *Store) RequestReplanning(ctx context.Context, lease AgentTurnLease, operation string, concern collaboration.ReplanningConcern) (collaboration.ReplanningCycle, error) {
	var cycle collaboration.ReplanningCycle
	err := s.withLockedAgentTurnLease(ctx, lease, "request replanning", func(tx pgx.Tx, turn lockedTurn) error {
		if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
			return ErrMutationAdmissionClosed
		}
		snapshot, err := rehydrateWorkflow(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if snapshot.State != workflow.StateDeveloping || snapshot.PlanningOnly || snapshot.ActiveTurn == nil || snapshot.ActiveTurn.ID != turn.ID || snapshot.ActiveTurn.Role != workflow.RoleDeveloper {
			return collaboration.ErrInvalid
		}
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_invocations WHERE id=$1 AND agent_turn_id=$2 AND execution_epoch=$3 AND tool_name='request_replanning' AND state='IN_FLIGHT')`, operation, turn.ID, turn.ExecutionEpoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return collaboration.ErrInvalid
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		var planner string
		err = tx.QueryRow(ctx, `SELECT planner.id::text FROM agent_participants developer JOIN agent_participants planner ON planner.workflow_id=developer.workflow_id AND planner.generation=developer.generation AND planner.role='PLANNER' WHERE developer.id=$1 AND developer.role='DEVELOPER'`, turn.AgentParticipantID).Scan(&planner)
		if errors.Is(err, pgx.ErrNoRows) {
			return collaboration.ErrInvalid
		}
		if err != nil {
			return err
		}
		head := turn.ExpectedHeadSHA
		if head == "" {
			head = turn.AgentProfileCommitSHA
		}
		var published string
		err = tx.QueryRow(ctx, `SELECT result->>'head' FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='publish_changes' AND state='SUCCEEDED' AND result->>'head' IS NOT NULL ORDER BY invocation_number DESC LIMIT 1`, turn.ID, turn.ExecutionEpoch).Scan(&published)
		if err == nil {
			head = published
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		scope := collaboration.ReplanningRequestScope{OperationID: operation, TurnID: turn.ID, ExecutionEpoch: uint64(turn.ExecutionEpoch), AttemptID: turn.WorkflowAttemptID, DeveloperParticipantID: turn.AgentParticipantID, PlannerParticipantID: planner, HeadSHA: head}
		if snapshot.ChangeProposal != nil {
			scope.ChangeProposalID = snapshot.ChangeProposal.ID
		}
		cycle, err = state.RequestReplanning(scope, concern)
		if err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
	return cycle, err
}

func (s *Store) GetReplanningRequest(ctx context.Context, workflowID, operation string) (collaboration.ReplanningCycle, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw); err != nil {
		return collaboration.ReplanningCycle{}, err
	}
	var state collaboration.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return collaboration.ReplanningCycle{}, err
	}
	if state.Replanning != nil {
		for _, c := range state.Replanning.Cycles {
			if c.Scope.OperationID == operation {
				return c, nil
			}
		}
	}
	return collaboration.ReplanningCycle{}, collaboration.ErrInvalid
}
