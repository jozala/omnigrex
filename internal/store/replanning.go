package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/publication"
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
		var reserved []byte
		var caller string
		if err = tx.QueryRow(ctx, `SELECT request,operation_id FROM tool_invocations WHERE id=$1`, operation).Scan(&reserved, &caller); err != nil {
			return err
		}
		var bound collaboration.ReplanningConcern
		var identity struct {
			OperationID string `json:"operation_id"`
		}
		if json.Unmarshal(reserved, &bound) != nil || json.Unmarshal(reserved, &identity) != nil || identity.OperationID != caller || !bound.Equal(concern) {
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
		} else {
			// Preserve a PR opened as unfinished work in this very Turn. Settlement's
			// existing publication-association proof still validates the complete chain.
			var opened []byte
			err = tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='open_pr' AND state='SUCCEEDED' ORDER BY invocation_number DESC LIMIT 1`, turn.ID, turn.ExecutionEpoch).Scan(&opened)
			if err == nil {
				v, e := publication.DecodeOpenResult(opened)
				if e != nil {
					return e
				}
				scope.ChangeProposalID = v.PullRequestID
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
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

func validateReplanningReceiptTx(ctx context.Context, tx pgx.Tx, workflowID, operation string, args, result []byte) error {
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	var concern collaboration.ReplanningConcern
	var receipt struct {
		Outcome string `json:"outcome"`
		CycleID string `json:"cycle_id"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(args, &concern) != nil || json.Unmarshal(result, &receipt) != nil || receipt.Outcome != "REPLANNING_REQUESTED" || receipt.CycleID != operation || receipt.Status != "recorded" {
		return ErrTerminalCorroborationConflict
	}
	if state.Replanning != nil {
		for _, c := range state.Replanning.Cycles {
			if c.Scope.OperationID == operation && c.Concern.Equal(concern) {
				return nil
			}
		}
	}
	return ErrTerminalCorroborationConflict
}

// CompleteReplanning extends the existing complete_planning capability. It
// records resolution evidence; settlement and admission still recheck authority.
func (s *Store) CompleteReplanning(ctx context.Context, lease AgentTurnLease, operation, revision, commit, evidence, disposition string, resolved bool) error {
	return s.withLockedAgentTurnLease(ctx, lease, "complete replanning", func(tx pgx.Tx, turn lockedTurn) error {
		if err := planningInvocation(ctx, tx, turn, operation, "complete_planning"); err != nil {
			return err
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		delivered, err := acceptedPlanDeliveredTx(ctx, tx, turn.ID)
		if err != nil {
			return err
		}
		if !delivered {
			return collaboration.ErrInvalid
		}
		cycle := state.ActiveReplanning()
		if cycle == nil || !resolved || cycle.Scope.PlannerParticipantID != turn.AgentParticipantID {
			return collaboration.ErrInvalid
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='refresh_default_branch' AND state='SUCCEEDED' ORDER BY invocation_number DESC LIMIT 1`, turn.ID, turn.ExecutionEpoch).Scan(&raw); err != nil {
			return err
		}
		var base struct {
			Status string `json:"status"`
			SHA    string `json:"sha"`
		}
		if json.Unmarshal(raw, &base) != nil || base.Status != "current" || base.SHA != commit {
			return collaboration.ErrInvalid
		}
		workHead := ""
		var proposalID int64
		err = tx.QueryRow(ctx, `SELECT pull_request_id,head_sha FROM change_proposals WHERE workflow_id=$1 AND active`, lease.JobLease.WorkflowID).Scan(&proposalID, &workHead)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if proposalID > 0 {
			if err = tx.QueryRow(ctx, `SELECT result FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='get_pull_request' AND state='SUCCEEDED' ORDER BY invocation_number DESC LIMIT 1`, turn.ID, turn.ExecutionEpoch).Scan(&raw); err != nil {
				return err
			}
			var pr struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
				Head  struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}
			if json.Unmarshal(raw, &pr) != nil || pr.ID != proposalID || pr.State != "open" || pr.Head.SHA != workHead {
				return collaboration.ErrInvalid
			}
		}
		grant := state.ImplementationGrant()
		if disposition == "revised" || disposition == "unchanged" && grant != nil && grant.ID != state.Planning.Implementation.AuthorizationID {
			if err = state.ValidatePlanning(operation, revision, turn.AgentParticipantID, turn.ID, commit, evidence, true); err != nil {
				return err
			}
			state.Planning.ReadyRevision = revision
		}
		if err = state.ResolveReplanning(turn.AgentParticipantID, collaboration.ReplanningResolution{OperationID: operation, TurnID: turn.ID, RevisionID: revision, RepositoryCommit: commit, WorkHeadSHA: workHead, Evidence: evidence, Disposition: disposition}); err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
}
func (s *Store) GetReplanningResolution(ctx context.Context, workflowID, operation string) (collaboration.ReplanningResolution, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT r FROM work_item_collaboration c,LATERAL jsonb_array_elements(c.state->'replanning'->'cycles') cycle,LATERAL jsonb_array_elements(cycle->'resolutions') r WHERE c.workflow_id=$1 AND r->>'operation_id'=$2`, workflowID, operation).Scan(&raw); err != nil {
		return collaboration.ReplanningResolution{}, err
	}
	var v collaboration.ReplanningResolution
	err := json.Unmarshal(raw, &v)
	return v, err
}
