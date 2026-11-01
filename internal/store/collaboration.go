package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
)

var ErrUnresolvedClarification = errors.New("clarification questions remain unresolved; record explicit interpretation with resolve_clarification or publish a revised round")

var ErrPendingHumanInput = errors.New("pending human input must be delivered before Stage handoff; retrieve all frozen human_input chunks, or call request_clarification with empty questions for input accepted after preparation")

func readCollaborationTx(ctx context.Context, tx pgx.Tx, workflowID string) (collaboration.State, error) {
	var state collaboration.State
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}
func saveCollaborationTx(ctx context.Context, tx pgx.Tx, workflowID string, state collaboration.State) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2) ON CONFLICT(workflow_id) DO UPDATE SET state=EXCLUDED.state,updated_at=clock_timestamp()`, workflowID, raw)
	return err
}

// RequestClarification reserves domain identities before any human-visible effect.
// Its exact live Turn fence and operation ledger make retry after a lost response safe.
func (s *Store) RequestClarification(ctx context.Context, lease AgentTurnLease, operation string, questions []collaboration.Question) (collaboration.Round, error) {
	var round collaboration.Round
	err := s.withLockedAgentTurnLease(ctx, lease, "reserve clarification", func(tx pgx.Tx, turn lockedTurn) error {
		if !turn.MutationAdmissionOpen || turn.Status != AgentTurnRunning {
			return ErrMutationAdmissionClosed
		}
		var mutationID, head string
		if err := tx.QueryRow(ctx, `SELECT id::text,COALESCE(expected_sha,'') FROM tool_invocations WHERE agent_turn_id=$1 AND execution_epoch=$2 AND tool_name='request_clarification' AND id=$3 AND state='IN_FLIGHT'`, turn.ID, turn.ExecutionEpoch, operation).Scan(&mutationID, &head); err != nil {
			return err
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if len(questions) == 0 && state.Pending(turn.AgentParticipantID) {
			return nil
		}
		id := "R" + mutationID
		for _, r := range state.Rounds {
			if r.ID == id {
				round = r
				return nil
			}
		}
		// A proven failed POST must not strand later clarification after explicit
		// recovery. Unknown effects remain fenced until artifact reconciliation.
		for _, prior := range state.Rounds {
			if prior.Published || prior.PublicationFailed {
				continue
			}
			var failed bool
			if err = tx.QueryRow(ctx, `SELECT state='FAILED' FROM tool_invocations WHERE id=$1`, strings.TrimPrefix(prior.ID, "R")).Scan(&failed); err != nil {
				return err
			}
			if failed {
				if err = state.FailPublication(prior.ID); err != nil {
					return err
				}
			}
		}
		round, err = state.Request(id, turn.AgentParticipantID, turn.ID, head, questions)
		if err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
	return round, err
}

// ConfirmClarificationPublication is called only after the adapter has verified
// the exact operation-marked artifact, including during mutation recovery.
func (s *Store) ConfirmClarificationPublication(ctx context.Context, workflowID, roundID string, source collaboration.Source) error {
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
	if err = state.Publish(roundID, source); err != nil {
		return err
	}
	if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) GetClarificationRound(ctx context.Context, workflowID, roundID string) (collaboration.Round, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, workflowID).Scan(&raw); err != nil {
		return collaboration.Round{}, err
	}
	var state collaboration.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return collaboration.Round{}, err
	}
	for _, round := range state.Rounds {
		if round.ID == roundID {
			return round, nil
		}
	}
	return collaboration.Round{}, collaboration.ErrInvalid
}

// HumanInputSubmission is the channel-neutral authorized ingestion operation.
// The adapter verifies actor permission before calling; state and routing are
// always validated again under the same Workflow lock as Stage settlement.
type HumanInputSubmission struct {
	WorkflowID string
	Command    collaboration.Command
	Actor      collaboration.Actor
	Source     collaboration.Source
	Authorized bool
	At         time.Time
}

func (s *Store) AdmitHumanInput(ctx context.Context, input HumanInputSubmission) (collaboration.Receipt, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return collaboration.Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	snapshot, err := rehydrateWorkflow(ctx, tx, input.WorkflowID)
	if err != nil {
		return collaboration.Receipt{}, err
	}
	state, err := readCollaborationTx(ctx, tx, input.WorkflowID)
	if err != nil {
		return collaboration.Receipt{}, err
	}
	active := snapshot.CurrentAttempt != nil && (snapshot.State == workflow.StateDeveloping || snapshot.State == workflow.StateReviewing)
	participant := ""
	if active {
		err = tx.QueryRow(ctx, `SELECT assignment.agent_participant_id::text FROM stage_assignments assignment JOIN agent_participants participant ON participant.id=assignment.agent_participant_id WHERE assignment.workflow_id=$1 AND assignment.stage_id=$2 AND participant.status='ACTIVE' ORDER BY assignment.assignment_generation DESC LIMIT 1`, input.WorkflowID, snapshot.CurrentAttempt.CurrentStage).Scan(&participant)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return collaboration.Receipt{}, err
		}
	}
	id, err := randomUUID()
	if err != nil {
		return collaboration.Receipt{}, err
	}
	prior := len(state.Inputs)
	receipt := state.Accept("A"+id, input.Command, collaboration.Admission{Active: active, Authorized: input.Authorized, ParticipantID: participant, Route: collaborationRoute(snapshot), Actor: input.Actor, Source: input.Source, At: input.At})
	if len(state.Inputs) > prior && snapshot.ActiveTurn == nil {
		if err = s.ensureHumanInputTurnTx(ctx, tx, input.WorkflowID, snapshot, receipt.ResponseID); err != nil {
			return collaboration.Receipt{}, err
		}
	}
	if err = saveCollaborationTx(ctx, tx, input.WorkflowID, state); err != nil {
		return collaboration.Receipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return collaboration.Receipt{}, err
	}
	return receipt, nil
}

func (s *Store) ensureHumanInputTurnTx(ctx context.Context, tx pgx.Tx, workflowID string, snapshot workflow.Snapshot, identity string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE workflow_id=$1 AND kind IN ('PREPARE_AGENT_TURN','REVALIDATE_TERMINAL_INTENT') AND status IN ('AVAILABLE','LEASED'))`, workflowID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return nil
	}
	stage, ok := s.reducer.Definition().Stage(snapshot.CurrentAttempt.CurrentStage)
	if !ok {
		return ErrWorkflowDecisionInvalid
	}
	head := ""
	if snapshot.ChangeProposal != nil {
		head = snapshot.ChangeProposal.HeadSHA
	}
	payload := map[string]any{"mode": workflow.AssignmentGenerationCurrent, "stage": stage.ID, "role": stage.Role, "purpose": workflow.TurnPurposeReactivation, "expected_head_sha": head, "retry_of_turn_id": "", "revision": snapshot.Revision}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = insertJobTx(ctx, tx, jobInsert{queue: WorkflowActionQueue, kind: PrepareAgentTurnJobKind, payload: raw, maxAttempts: 3, idempotencyKey: "human-input:" + identity, scope: jobInsertScope{workflowID: workflowID, workflowAttemptID: snapshot.CurrentAttempt.ID}})
	return err
}

func prepareCollaborationTx(ctx context.Context, tx pgx.Tx, workflowID, participant, turn, route string, configuration json.RawMessage) (json.RawMessage, error) {
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return nil, err
	}
	if len(state.Inputs) == 0 && len(state.Rounds) == 0 {
		return configuration, nil
	}
	for i := range state.Inputs {
		if state.Inputs[i].ParticipantID == "" && state.Inputs[i].Route == route {
			state.Inputs[i].ParticipantID = participant
		}
	}
	state.Prepare(participant, turn)
	state.Waiting = false
	var object map[string]json.RawMessage
	if err = json.Unmarshal(configuration, &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid Turn configuration")
	}
	for name, raw := range state.Project(participant, turn) {
		object[name] = raw
	}
	if err = saveCollaborationTx(ctx, tx, workflowID, state); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func (s *Store) ResolveClarification(ctx context.Context, lease AgentTurnLease, round string, resolutions []collaboration.Resolution) error {
	return s.withLockedAgentTurnLease(ctx, lease, "resolve clarification", func(tx pgx.Tx, turn lockedTurn) error {
		if !turn.MutationAdmissionOpen {
			return ErrMutationAdmissionClosed
		}
		state, err := readCollaborationTx(ctx, tx, lease.JobLease.WorkflowID)
		if err != nil {
			return err
		}
		if err = state.Resolve(round, turn.ID, resolutions); err != nil {
			return err
		}
		return saveCollaborationTx(ctx, tx, lease.JobLease.WorkflowID, state)
	})
}

// FindCollaborationWorkItem resolves an exact Issue, never a Pull Request,
// without creating a Workflow as a side effect of human input.
func (s *Store) FindCollaborationWorkItem(ctx context.Context, repositoryID, issueID, issueNumber int64) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM workflows WHERE repository_id=$1 AND issue_id=$2 AND issue_number=$3`, repositoryID, issueID, issueNumber).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func fenceCollaborationSettlementTx(ctx context.Context, tx pgx.Tx, workflowID, participant string, event *workflow.TurnSettledEvent) error {
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	event.HumanInputPending = state.Pending(participant)
	if event.HumanInputPending && (event.Outcome == workflow.TurnOutcomeChangeProposalReady || event.Outcome == workflow.TurnOutcomeApproved || event.Outcome == workflow.TurnOutcomeChangesRequested) {
		event.Outcome = workflow.TurnOutcomeClarificationWait
		event.Review = nil
		event.ExistingReview = nil
		event.AuthorizedReviewerActorID = 0
	}
	if event.Outcome == workflow.TurnOutcomeClarificationWait {
		state.Waiting = true
		return saveCollaborationTx(ctx, tx, workflowID, state)
	}
	return nil
}

func collaborationRoute(snapshot workflow.Snapshot) string {
	if snapshot.CurrentAttempt == nil {
		return ""
	}
	return snapshot.CurrentAttempt.ID + ":" + string(snapshot.CurrentAttempt.CurrentStage)
}

func (s *Store) HasPendingHumanInput(ctx context.Context, workflowID, participant string) (bool, error) {
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
	return state.Pending(participant), nil
}

// GetCollaborationRecord reads immutable accepted contents without projecting an
// unbounded history into the prompt. Counts are frozen at Turn preparation.
func (s *Store) GetCollaborationRecord(ctx context.Context, workflowID, participant, turn, collection string, index, offset int) (collaboration.RecordPage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return collaboration.RecordPage{}, err
	}
	defer tx.Rollback(ctx)
	raw, total, err := getCollaborationRecordTx(ctx, tx, workflowID, participant, turn, collection, index)
	if err != nil {
		return collaboration.RecordPage{}, err
	}
	return collaboration.PageRecord(raw, total, index, offset)
}

func getCollaborationRecordTx(ctx context.Context, tx pgx.Tx, workflowID, participant, turn, collection string, index int) (json.RawMessage, int, error) {
	var configuration []byte
	err := tx.QueryRow(ctx, `SELECT turn.turn_configuration FROM agent_turns turn JOIN agent_sessions session ON session.id=turn.agent_session_id WHERE turn.id=$1 AND turn.workflow_id=$2 AND session.agent_participant_id=$3`, turn, workflowID, participant).Scan(&configuration)
	if err != nil {
		return nil, 0, err
	}
	var object struct {
		Projection collaboration.Projection `json:"collaboration_projection"`
	}
	if err = json.Unmarshal(configuration, &object); err != nil {
		return nil, 0, err
	}
	limit, ok := object.Projection.Counts[collection]
	if !ok {
		return nil, 0, collaboration.ErrInvalid
	}
	if index < 0 || index >= limit {
		return nil, 0, collaboration.ErrInvalid
	}
	// Select just this record: repeatedly decoding and serializing the entire
	// aggregate for each chunk makes large discussions unnecessarily expensive.
	var raw []byte
	switch collection {
	case "human_input", "human_input_history":
		err = tx.QueryRow(ctx, `SELECT item - 'delivery_pending' FROM work_item_collaboration collaboration,
   LATERAL jsonb_array_elements(collaboration.state->'inputs') WITH ORDINALITY AS records(item,position)
   WHERE collaboration.workflow_id=$1 AND item->>'participant_id'=$2 AND COALESCE(item->>'delivered_to','')<>''
   AND (($3='human_input' AND item->>'delivered_to'=$4) OR ($3='human_input_history' AND item->>'delivered_to'<>$4))
   ORDER BY position OFFSET $5 LIMIT 1`, workflowID, participant, collection, turn, index).Scan(&raw)
	case "clarification_history":
		err = tx.QueryRow(ctx, `SELECT state->'rounds'->$2::int FROM work_item_collaboration WHERE workflow_id=$1`, workflowID, index).Scan(&raw)
	default:
		return nil, 0, collaboration.ErrInvalid
	}
	if err != nil {
		return nil, 0, err
	}
	return raw, limit, nil
}

// recordCollaborationDeliveryTx admits coverage only from a successful read
// recorded under the live Turn/Workflow fence. The read ledger is the durable
// progress log; duplicate, missing, failed, and noncanonical chunks cannot
// acknowledge an input. No aggregate rewrite is needed for partial reads.
func recordCollaborationDeliveryTx(ctx context.Context, tx pgx.Tx, turn lockedTurn, workflowID string, request, result json.RawMessage) error {
	var args struct {
		Collection string `json:"collection"`
		Index      int    `json:"index"`
		Offset     int    `json:"offset"`
	}
	if err := json.Unmarshal(request, &args); err != nil {
		return err
	}
	if args.Collection != "human_input" {
		return nil
	}
	raw, total, err := getCollaborationRecordTx(ctx, tx, workflowID, turn.AgentParticipantID, turn.ID, args.Collection, args.Index)
	if err != nil {
		return err
	}
	var input struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	expected, err := collaboration.PageRecord(raw, total, args.Index, args.Offset)
	if err != nil {
		return err
	}
	var actual collaboration.RecordPage
	if err = json.Unmarshal(result, &actual); err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, actual) {
		return collaboration.ErrInvalid
	}
	var chunks int
	length := utf8.RuneCount(raw)
	err = tx.QueryRow(ctx, `SELECT count(DISTINCT COALESCE((request->>'offset')::bigint,0))
 FROM tool_invocations WHERE agent_turn_id=$1 AND kind='READ' AND state='SUCCEEDED'
 AND tool_name='get_collaboration_record' AND request->>'collection'='human_input'
 AND COALESCE((request->>'index')::bigint,0)=$2 AND result->>'record_sha256'=$3
 AND COALESCE((request->>'offset')::bigint,0)>=0 AND COALESCE((request->>'offset')::bigint,0)<$4
 AND COALESCE((request->>'offset')::bigint,0)%$5=0`, turn.ID, args.Index, expected.RecordSHA256, length, collaboration.RecordChunkRunes).Scan(&chunks)
	if err != nil {
		return err
	}
	if chunks != (length+collaboration.RecordChunkRunes-1)/collaboration.RecordChunkRunes {
		return nil
	}
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	for _, in := range state.Inputs {
		if in.ID == input.ID && !in.DeliveryPending {
			return nil
		}
	}
	if err = state.MarkDelivered(turn.AgentParticipantID, turn.ID, input.ID); err != nil {
		return err
	}
	return saveCollaborationTx(ctx, tx, workflowID, state)
}
