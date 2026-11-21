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
	if !preImplementation(*state) && state.ActiveReplanning() == nil {
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
	if state.ActiveReplanning() != nil {
		if err := state.ReconsiderReplanning(); err != nil {
			return snapshot, err
		}
	} else {
		state.Planning.ReadyRevision = ""
	}
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
	if c := state.ActiveReplanning(); c != nil && c.Phase != collaboration.ReplanningRequested && payload.Role == workflow.RoleDeveloper {
		head := ""
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT head_sha FROM change_proposals WHERE workflow_id=$1 AND active),'')`, job.WorkflowID).Scan(&head); err != nil {
			return false, err
		}
		if state.ResumptionReady() && c.Resolution.RepositoryCommit == commit && c.Resolution.WorkHeadSHA == head {
			if c.Resolution.AuthorizationID != state.Planning.Implementation.AuthorizationID && verifiedAuthorization != c.Resolution.AuthorizationID {
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
		return true, s.ensureHumanInputTurnTx(ctx, tx, job.WorkflowID, snapshot, job.ID)
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
	if err != nil {
		return err
	}
	if state.ActiveReplanning() != nil {
		if err = state.AdmitResumption(turnID); err != nil {
			return err
		}
		r := state.CurrentCheckpoint()
		if err = enqueueCollaborationStatusTx(ctx, tx, workflowID, "resumed:"+turnID, "Developer implementation resumed under "+r.Reference+". The same Work Item and Change Proposal remain in use; prior publication history and review/replanning usage are preserved.", &state); err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, workflowID, state)
	}
	if !preImplementation(state) {
		return nil
	}
	if !state.ImplementationValidated() {
		return collaboration.ErrInvalid
	}
	grant := state.ImplementationGrant()
	state.Planning.Implementation.FirstDeveloperTurnID = turnID
	state.Planning.Implementation.AcceptedRevisionID = grant.RevisionID
	state.Planning.Implementation.AuthorizationID = grant.ID
	state.Planning.Implementation.Bindings = append(state.Planning.Implementation.Bindings, collaboration.ImplementationBinding{TurnID: turnID, RevisionID: grant.RevisionID, AuthorizationID: grant.ID})
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
	raw, _ := json.Marshal(map[string]any{"authorization": authorization, "revision_id": state.Planning.Implementation.AcceptedRevisionID, "authorization_id": state.Planning.Implementation.AuthorizationID, "instructions": "Read the exact complete accepted pair in accepted_plan (or planning_history) and original request in planning_request before implementation or review. Follow accepted behavior and material design commitments; reversible local suggestions may be adjusted. Retrieve original decisions, evidence and scoped history as needed. Material contradictions must not silently change accepted intent. Developers use an evidence-backed request_replanning to the retained Planner; Reviewers retain their existing review/Human Handoff path, not Developer-to-Planner authority. Read the scoped replanning concern, resolution and original evidence as well as the exact governing pair. Direct development without a retained Planner still uses clarification or Human Handoff."})
	return raw
}

// Inline exact documents count as delivery. Omitted documents require every
// canonical successful read chunk before terminal handoff, just like human input.
func acceptedPlanDeliveredTx(ctx context.Context, tx pgx.Tx, turnID string) (bool, error) {
	var configuration []byte
	if err := tx.QueryRow(ctx, `SELECT turn_configuration FROM agent_turns WHERE id=$1`, turnID).Scan(&configuration); err != nil {
		return false, err
	}

	var config map[string]json.RawMessage
	if err := json.Unmarshal(configuration, &config); err != nil {
		return false, err
	}
	if len(config["collaboration_projection"]) == 0 {
		return true, nil
	}
	var projection collaboration.Projection
	if err := json.Unmarshal(config["collaboration_projection"], &projection); err != nil {
		return false, err
	}
	var participant, workflowID string
	if err := tx.QueryRow(ctx, `SELECT t.workflow_id::text,s.agent_participant_id::text FROM agent_turns t JOIN agent_sessions s ON s.id=t.agent_session_id WHERE t.id=$1`, turnID).Scan(&workflowID, &participant); err != nil {
		return false, err
	}
	for _, collection := range []string{"accepted_plan", "planning_request", "replanning"} {
		// Each complete accepted revision embeds the exact original request/source;
		// do not require a duplicate read of the same original body.
		if collection == "planning_request" && projection.Counts["accepted_plan"] > 0 {
			continue
		}
		if projection.Counts[collection] == 0 {
			continue
		}
		var inline []json.RawMessage
		if json.Unmarshal(config[collection], &inline) == nil && len(inline) > 0 {
			continue
		}
		raw, total, err := getCollaborationRecordTx(ctx, tx, workflowID, participant, turnID, collection, 0)
		if err != nil {
			return false, err
		}
		for offset := 0; ; offset += collaboration.RecordChunkRunes {
			page, err := collaboration.PageRecord(raw, total, 0, offset)
			if err != nil {
				return false, err
			}
			var found bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_invocations WHERE agent_turn_id=$1 AND kind='READ' AND state='SUCCEEDED' AND tool_name='get_collaboration_record' AND request->>'collection'=$4 AND COALESCE((request->>'index')::int,0)=0 AND COALESCE((request->>'offset')::int,0)=$2 AND result->>'record_sha256'=$3)`, turnID, offset, page.RecordSHA256, collection).Scan(&found); err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			if page.NextOffset == nil {
				break
			}
		}
	}
	return true, nil
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
	if c := state.ActiveReplanning(); c != nil && c.Phase == collaboration.ReplanningQueued && c.Resolution != nil {
		if c.Resolution.AuthorizationID == state.Planning.Implementation.AuthorizationID {
			return nil, nil
		}
		grant = nil
		for _, a := range state.Planning.Implementation.Actions {
			if a.ID == c.Resolution.AuthorizationID {
				copy := a
				grant = &copy
				break
			}
		}
	}
	if grant == nil {
		return nil, nil
	}
	request := &ImplementationAdmissionRequest{AuthorizationID: grant.ID, Actor: grant.Actor}
	err = s.pool.QueryRow(ctx, `SELECT repository_id,issue_id,issue_number FROM workflows WHERE id=$1`, workflowID).Scan(&request.WorkItem.RepositoryID, &request.WorkItem.IssueID, &request.WorkItem.IssueNumber)
	return request, err
}
