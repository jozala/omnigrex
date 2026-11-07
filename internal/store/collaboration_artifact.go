package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
)

// SaveCollaborationArtifact commits exact bytes before the first external POST.
// It is not permitted to replace a payload when a renderer changes on restart.
func (s *Store) SaveCollaborationArtifact(ctx context.Context, lease AgentTurnLease, id string, artifact collaboration.RenderedPublication) error {
	return s.withLockedAgentTurnLease(ctx, lease, "freeze collaboration publication", func(tx pgx.Tx, turn lockedTurn) error {
		if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
			return ErrMutationAdmissionClosed
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		var target **collaboration.RenderedPublication
		for i := range state.Rounds {
			r := &state.Rounds[i]
			if r.ID == id && r.TurnID == turn.ID && r.ParticipantID == turn.AgentParticipantID && !r.PublicationFailed {
				target = &r.Artifact
			}
		}
		if state.Planning != nil {
			for i := range state.Planning.Revisions {
				r := &state.Planning.Revisions[i]
				if r.ID == id && r.TurnID == turn.ID && r.ParticipantID == turn.AgentParticipantID && !r.PublicationFailed {
					target = &r.Artifact
				}
			}
		}
		if target == nil || artifact.Body == "" || artifact.Marker == "" {
			return collaboration.ErrInvalid
		}
		if *target != nil {
			if **target == artifact {
				return nil
			}
			return ErrMutationOperationConflict
		}
		*target = &artifact
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
}

// ReconcileClarificationNonPublication proves absence of an external effect
// only after stopping and fencing the exact old Turn. A persisted artifact
// means a POST may have started and must stay on exact-artifact reconciliation.
func (s *Store) ReconcileClarificationNonPublication(ctx context.Context, workflowID, turnID, operation string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = rehydrateWorkflow(ctx, tx, workflowID); err != nil {
		return false, err
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return false, err
	}
	for _, r := range state.Rounds {
		if r.ID == "R"+operation && (r.Published || r.Artifact != nil) {
			return false, nil
		}
	}
	var stopped bool
	if err = tx.QueryRow(ctx, `SELECT t.runtime_stopped_at IS NOT NULL AND NOT t.mutation_admission_open AND t.mutation_admission_closed_at IS NOT NULL FROM agent_turns t JOIN tool_invocations i ON i.agent_turn_id=t.id AND i.execution_epoch=t.execution_epoch WHERE t.workflow_id=$1 AND t.id=$2 AND i.id=$3 AND i.tool_name='request_clarification' AND i.state IN ('UNKNOWN','RECONCILING') FOR UPDATE OF t,i`, workflowID, turnID, operation).Scan(&stopped); err != nil {
		return false, err
	}
	if !stopped {
		return false, ErrAgentTurnRecoveryUnsettled
	}
	for _, r := range state.Rounds {
		if r.ID == "R"+operation {
			if r.TurnID != turnID {
				return false, collaboration.ErrInvalid
			}
			if err = state.FailPublication(r.ID); err != nil {
				return false, err
			}
		}
	}
	if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
