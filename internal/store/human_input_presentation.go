package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
)

// humanInputWaiting is an observation, never a scheduling or admission decision.
func humanInputWaiting(status workflow.State, settled, active, continuation bool, participant string, state collaboration.State) bool {
	if status != workflow.StatePlanning && status != workflow.StateDeveloping && status != workflow.StateReviewing {
		return false
	}
	if !settled || !state.Waiting || active || continuation || participant == "" || state.Pending(participant) {
		return false
	}
	if pair := state.CurrentCheckpoint(); state.AwaitingPlanConfirmation(participant) && pair.Source != nil && pair.Source.ID != "" && !pair.PublicationFailed {
		return true
	}
	for _, round := range state.Rounds {
		if round.ID == state.CurrentRound && round.ParticipantID == participant && round.Published && round.Source != nil && round.Source.ID != "" && !round.PublicationFailed && round.SupersededBy == "" && len(round.Questions) > 0 {
			return true
		}
	}
	return false
}

// refreshHumanInputPresentationTx runs at locked collaboration, lifecycle and
// settlement boundaries. The cache is only for invalidation; workers recompute
// from current records, including when processing migration-seeded jobs.
func refreshHumanInputPresentationTx(ctx context.Context, tx pgx.Tx, workflowID string) error {
	var status workflow.State
	var settled, previous bool
	if err := tx.QueryRow(ctx, `SELECT status, clarification_waiting, human_input_waiting
FROM workflows WHERE id=$1 FOR UPDATE`, workflowID).Scan(&status, &settled, &previous); err != nil {
		return err
	}
	desired := false
	if settled && (status == workflow.StatePlanning || status == workflow.StateDeveloping || status == workflow.StateReviewing) {
		var participant, attempt string
		var active, continuation bool
		if err := tx.QueryRow(ctx, `SELECT
    COALESCE((SELECT id::text FROM workflow_attempts WHERE workflow_id=$1 AND active), ''),
    COALESCE((SELECT a.agent_participant_id::text
        FROM stage_assignments a JOIN agent_participants p ON p.id=a.agent_participant_id
        JOIN workflow_attempts w ON w.workflow_id=a.workflow_id AND w.active AND w.current_stage=a.stage_id
        WHERE a.workflow_id=$1 AND p.status='ACTIVE'
        ORDER BY a.assignment_generation DESC LIMIT 1), ''),
    EXISTS(SELECT 1 FROM agent_turns WHERE workflow_id=$1 AND active),
    EXISTS(SELECT 1 FROM jobs WHERE workflow_id=$1
        AND kind IN ('PREPARE_AGENT_TURN','REVALIDATE_TERMINAL_INTENT')
        AND status IN ('AVAILABLE','LEASED'))`, workflowID).Scan(&attempt, &participant, &active, &continuation); err != nil {
			return err
		}
		// Most collaboration saves occur during a Turn. Avoid decoding a potentially
		// large history a second time when execution already excludes waiting.
		if attempt != "" && participant != "" && !active && !continuation {
			state, err := readCollaborationTx(ctx, tx, workflowID)
			if err != nil {
				return err
			}
			desired = humanInputWaiting(status, settled, active, continuation, participant, state)
		}
	}
	if desired == previous {
		return nil
	}
	var version, revision int64
	if err := tx.QueryRow(ctx, `UPDATE workflows
SET human_input_waiting=$2, human_input_presentation_revision=human_input_presentation_revision+1
WHERE id=$1 RETURNING human_input_presentation_revision, state_revision`, workflowID, desired).Scan(&version, &revision); err != nil {
		return err
	}
	return enqueueHumanInputPresentationTx(ctx, tx, workflowID, fmt.Sprintf("human-input-presentation:%s:%d", workflowID, version), revision)
}

func enqueueHumanInputPresentationTx(ctx context.Context, tx pgx.Tx, workflowID, key string, revision int64) error {
	payload, _ := json.Marshal(map[string]any{"revision": revision, "human_input_only": true})
	_, err := insertIdempotentJobTx(ctx, tx, jobInsert{
		queue: WorkflowActionQueue, kind: ReconcileGitHubLabelsJobKind, payload: payload,
		maxAttempts: 3, idempotencyKey: key, scope: jobInsertScope{workflowID: workflowID},
	})
	return err
}
