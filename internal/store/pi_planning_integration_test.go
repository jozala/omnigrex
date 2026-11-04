//go:build integration

package store_test

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"testing"
	"time"
)

func testPiPlanningWorkflow(t *testing.T) {
	f := newPiWorkflowFixture(t, "planning")
	var participant, sessionID, attempt string
	for turn := 0; turn < 4; turn++ {
		prepared := f.prepare(t)
		if turn == 0 {
			participant = prepared.Commit.Participant.ID
			sessionID = prepared.Commit.Session.ID
			attempt = prepared.Commit.Turn.WorkflowAttemptID
		}
		if participant != prepared.Commit.Participant.ID || sessionID != prepared.Commit.Session.ID || attempt != prepared.Commit.Turn.WorkflowAttemptID || prepared.Commit.Participant.Role != workflow.RolePlanner {
			t.Fatal("lost continuity or allocated another Role")
		}
		if processed, err := f.worker.ProcessNext(f.ctx); err != nil || !processed {
			t.Fatalf("Planner Turn %d: %t %v", turn, processed, err)
		}
		if turn == 3 {
			break
		}
		var raw []byte
		var waiting bool
		var active, scheduled, failures int
		err := f.pool(t).QueryRow(f.ctx, `SELECT c.state,w.clarification_waiting,(SELECT count(*) FROM agent_turns WHERE workflow_id=w.id AND active),(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind IN ('RUN_AGENT_TURN','PREPARE_AGENT_TURN') AND status IN ('AVAILABLE','LEASED')),a.infrastructure_failures FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id JOIN workflow_attempts a ON a.workflow_id=w.id AND a.active WHERE w.id=$1`, f.workflowID).Scan(&raw, &waiting, &active, &scheduled, &failures)
		if err != nil || !waiting || active != 0 || scheduled != 0 || failures != 0 {
			t.Fatal("waiting retained execution or consumed retries", err, waiting, active, scheduled, failures)
		}
		var state collaboration.State
		if err = json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		command := collaboration.Command{Kind: "answer", Target: state.CurrentRound, Body: "Target small teams without external accounts."}
		if turn == 1 {
			command.Body = "Change the proposed retention assumption: retain no personal onboarding data. Use ephemeral local status."
		}
		if turn == 2 {
			r := state.CurrentCheckpoint()
			if r == nil || !r.Published || r.Content.Status != "proposed" || len(r.Decisions) != 2 {
				t.Fatal("missing paired proposal or decisions", state.Planning)
			}
			command = collaboration.Command{Kind: "confirm-plan", Target: r.ID}
		}
		receipt, err := f.database.AdmitHumanInput(f.ctx, store.HumanInputSubmission{WorkflowID: f.workflowID, Command: command, Actor: collaboration.Actor{ID: "human-fixture", Name: "Maintainer"}, Source: collaboration.Source{Channel: "qualification", ID: uuid.NewString()}, Authorized: true, At: time.Now()})
		if err != nil || receipt.ResponseID == "" {
			t.Fatal(receipt, err)
		}
	}
	f.assertState(t, workflow.StatePlanReady)
	var raw []byte
	var otherRoles, proposals, publication int
	err := f.pool(t).QueryRow(f.ctx, `SELECT state,(SELECT count(*) FROM agent_participants WHERE workflow_id=$1 AND role<>'PLANNER'),(SELECT count(*) FROM change_proposals WHERE workflow_id=$1),(SELECT count(*) FROM tool_invocations i JOIN agent_turns t ON t.id=i.agent_turn_id WHERE t.workflow_id=$1 AND i.tool_name IN ('publish_changes','open_pr','submit_review','request_review')) FROM work_item_collaboration WHERE workflow_id=$1`, f.workflowID).Scan(&raw, &otherRoles, &proposals, &publication)
	if err != nil || otherRoles != 0 || proposals != 0 || publication != 0 {
		t.Fatal("crossed implementation boundary", err, otherRoles, proposals, publication)
	}
	var state collaboration.State
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Planning.ReadyRevision == "" || len(state.Planning.Revisions) != 2 || len(state.Planning.Revalidations) != 1 || state.Planning.Revisions[1].Supersedes != state.Planning.Revisions[0].ID {
		t.Fatal("history missing", state.Planning)
	}
	t.Log("PASS: controlled retained Planner rounds, changed assumption, paired draft/proposal, exact confirmation and final revalidation; no implementation. This does not assess reasoning quality.")
}
