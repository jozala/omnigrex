package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
)

var ErrImplementationVerificationRequired = errors.New("exact implementation authorization requires fresh Issue and actor authority verification")

func preImplementation(state collaboration.State) bool {
	return state.Planning != nil && state.Planning.Implementation.FirstDeveloperTurnID == ""
}

// reopenPlanningTx shares the Workflow lock with withdrawal, closure, settlement
// and first admission. Queued or leased preparation is not implementation start.
func (s *Store) reopenPlanningTx(ctx context.Context, tx pgx.Tx, workflowID string, snapshot workflow.Snapshot, state *collaboration.State, identity string) (workflow.Snapshot, error) {
	if !preImplementation(*state) {
		return snapshot, collaboration.ErrInvalid
	}
	if snapshot.ActiveTurn != nil || snapshot.State == workflow.StatePlanning {
		return snapshot, nil
	}
	decision := s.reducer.ReopenPlanning(snapshot)
	if decision.Disposition != workflow.DispositionApplied {
		return snapshot, ErrWorkflowDecisionInvalid
	}
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return snapshot, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='CANCELLED',lease_owner=NULL,lease_token=NULL,leased_at=NULL,lease_expires_at=NULL,heartbeat_at=NULL,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')`, workflowID); err != nil {
		return snapshot, err
	}
	state.Planning.ReadyRevision = ""
	if err := persistAppliedDecision(ctx, tx, "", workflowID, snapshot, decision, "planning-input:"+identity); err != nil {
		return snapshot, err
	}
	return decision.Snapshot, nil
}

// fenceImplementationPreparationTx can redirect repository movement to Planner
// relevance validation without treating unrelated commits as material changes.
// This commits the redirection, not a partially allocated Participant or Turn.
func (s *Store) fenceImplementationPreparationTx(ctx context.Context, tx pgx.Tx, job Job, payload agentTurnPreparationPayload, commit, verifiedAuthorization string) (bool, error) {
	state, err := readCollaborationTx(ctx, tx, job.WorkflowID)
	if err != nil {
		return false, err
	}
	if !preImplementation(state) || payload.Role != workflow.RoleDeveloper {
		return false, nil
	}
	if state.ImplementationValidated() && state.ImplementationCommit() == commit {
		if grant := state.ImplementationGrant(); grant == nil || verifiedAuthorization != grant.ID {
			return false, ErrImplementationVerificationRequired
		}
		return false, nil
	}
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return false, err
	}
	snapshot, err = s.reopenPlanningTx(ctx, tx, job.WorkflowID, snapshot, &state, job.ID)
	if err != nil {
		return false, err
	}
	if err = saveCollaborationTx(ctx, tx, job.WorkflowID, state); err != nil {
		return false, err
	}
	if err = s.ensureHumanInputTurnTx(ctx, tx, job.WorkflowID, snapshot, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// consumeImplementationTx runs inside the exact transaction inserting the first
// Developer Turn. Rollback restores unused authority and budgets together.
func consumeImplementationTx(ctx context.Context, tx pgx.Tx, workflowID, turnID string, stage workflow.StageID) error {
	if stage != workflow.StageImplementation {
		return nil
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil || !preImplementation(state) {
		return err
	}
	if !state.ImplementationValidated() {
		return collaboration.ErrInvalid
	}
	grant := state.ImplementationGrant()
	state.Planning.Implementation.FirstDeveloperTurnID = turnID
	state.Planning.Implementation.AcceptedRevisionID = grant.RevisionID
	state.Planning.Implementation.AuthorizationID = grant.ID
	r := state.CurrentCheckpoint()
	if err = enqueueCollaborationStatusTx(ctx, tx, workflowID, "started:"+grant.ID, "Implementation of "+r.Reference+" started: the first Developer Agent Turn is durably admitted, including while its Execution Environment launches. Limited withdrawal no longer stops work; close the original Work Item Issue to use the existing stopping mechanism.", &state); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failures=0,review_usage='{}'::jsonb,updated_at=clock_timestamp() WHERE workflow_id=$1 AND active`, workflowID); err != nil {
		return err
	}
	return saveCollaborationTx(ctx, tx, workflowID, state)
}

// AcceptedPlanOrientation is deliberately small; exact documents and original
// sources remain in bounded planning_history/planning_request, not a summary.
func acceptedPlanOrientation(state collaboration.State) json.RawMessage {
	if state.Planning == nil || state.Planning.Implementation.FirstDeveloperTurnID == "" {
		return nil
	}
	var authorization *collaboration.ImplementationAction
	for _, a := range state.Planning.Implementation.Actions {
		if a.ID == state.Planning.Implementation.AuthorizationID {
			authorization = &a
			break
		}
	}
	raw, _ := json.Marshal(map[string]any{"authorization": authorization, "revision_id": state.Planning.Implementation.AcceptedRevisionID, "authorization_id": state.Planning.Implementation.AuthorizationID, "instructions": "Read the exact complete accepted pair in accepted_plan (or planning_history) and original request in planning_request before implementation or review. Follow accepted behavior and material design commitments; reversible local suggestions may be adjusted. Retrieve original decisions, evidence and scoped history as needed. Material contradictions preventing safe implementation require Human Handoff, not silent replanning."})
	return raw
}

// Inline exact documents count as delivery. Omitted documents require every
// canonical successful read chunk before terminal handoff, just like human input.
func acceptedPlanDeliveredTx(ctx context.Context, tx pgx.Tx, turnID string) (bool, error) {
	var configuration []byte
	if err := tx.QueryRow(ctx, `SELECT turn_configuration FROM agent_turns WHERE id=$1`, turnID).Scan(&configuration); err != nil {
		return false, err
	}
	var c struct {
		Projection collaboration.Projection `json:"collaboration_projection"`
		Accepted   []json.RawMessage        `json:"accepted_plan"`
	}
	if err := json.Unmarshal(configuration, &c); err != nil {
		return false, err
	}
	if c.Projection.Counts["accepted_plan"] == 0 || len(c.Accepted) > 0 {
		return true, nil
	}
	var participant, workflowID string
	if err := tx.QueryRow(ctx, `SELECT t.workflow_id::text,s.agent_participant_id::text FROM agent_turns t JOIN agent_sessions s ON s.id=t.agent_session_id WHERE t.id=$1`, turnID).Scan(&workflowID, &participant); err != nil {
		return false, err
	}
	raw, total, err := getCollaborationRecordTx(ctx, tx, workflowID, participant, turnID, "accepted_plan", 0)
	if err != nil {
		return false, err
	}
	for offset := 0; ; offset += collaboration.RecordChunkRunes {
		page, err := collaboration.PageRecord(raw, total, 0, offset)
		if err != nil {
			return false, err
		}
		var found bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_invocations WHERE agent_turn_id=$1 AND kind='READ' AND state='SUCCEEDED' AND tool_name='get_collaboration_record' AND request->>'collection'='accepted_plan' AND COALESCE((request->>'index')::int,0)=0 AND COALESCE((request->>'offset')::int,0)=$2 AND result->>'record_sha256'=$3)`, turnID, offset, page.RecordSHA256).Scan(&found); err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		if page.NextOffset == nil {
			return true, nil
		}
	}
}

// ImplementationAdmissionRequest binds fresh external observations to the exact
// durable grant. The Store rechecks this identity inside first admission.
type ImplementationAdmissionRequest struct {
	AuthorizationID string
	Actor           collaboration.Actor
	WorkItem        workflow.WorkItem
}

func (s *Store) GetImplementationAdmissionRequest(ctx context.Context, workflowID string) (*ImplementationAdmissionRequest, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state collaboration.State
	if err = json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	grant := state.ImplementationGrant()
	if grant == nil {
		return nil, nil
	}
	request := &ImplementationAdmissionRequest{AuthorizationID: grant.ID, Actor: grant.Actor}
	err = s.pool.QueryRow(ctx, `SELECT repository_id,issue_id,issue_number FROM workflows WHERE id=$1`, workflowID).Scan(&request.WorkItem.RepositoryID, &request.WorkItem.IssueID, &request.WorkItem.IssueNumber)
	return request, err
}
