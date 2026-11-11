//go:build integration

package store_test

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"testing"
	"time"
)

func testPiPlanningWorkflow(t *testing.T) {
	f := newPiWorkflowFixture(t, "planning")
	var participant, sessionID, attempt string
	for turn := 0; turn < 6; turn++ {
		prepared := f.prepare(t)
		if turn == 0 {
			participant = prepared.Commit.Participant.ID
			sessionID = prepared.Commit.Session.ID
			attempt = prepared.Commit.Turn.WorkflowAttemptID
		}
		if participant != prepared.Commit.Participant.ID || sessionID != prepared.Commit.Session.ID || attempt != prepared.Commit.Turn.WorkflowAttemptID || prepared.Commit.Participant.Role != workflow.RolePlanner {
			t.Fatal("lost continuity or allocated another Role")
		}
		if turn == 3 {
			// Preparation has frozen the revised third-round answer. These admissions finish
			// before the provider is released to attempt its stale checkpoint.
			var raw []byte
			if err := f.pool(t).QueryRow(f.ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, f.workflowID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var state collaboration.State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			for _, command := range []collaboration.Command{
				{Kind: "amend", Target: state.Inputs[0].Reference, Body: "Change the audience to users without repository access. Keep all status ephemeral."},
				{Kind: "steer", Body: "Use a standalone operator CLI, not a repository plugin."},
			} {
				receipt, err := f.database.AdmitHumanInput(f.ctx, store.HumanInputSubmission{WorkflowID: f.workflowID, Command: command, Actor: collaboration.Actor{ID: "human-fixture", Name: "Maintainer"}, Source: collaboration.Source{Channel: "qualification", ID: uuid.NewString()}, Authorized: true, At: time.Now()})
				if err != nil || receipt.ResponseID == "" {
					t.Fatal(receipt, err)
				}
			}
		}
		if processed, err := f.worker.ProcessNext(f.ctx); err != nil || !processed {
			t.Fatalf("Planner Turn %d: %t %v", turn, processed, err)
		}
		if turn == 5 {
			break
		}
		var raw []byte
		var waiting bool
		var active, scheduled, failures int
		err := f.pool(t).QueryRow(f.ctx, `SELECT c.state,w.clarification_waiting,(SELECT count(*) FROM agent_turns WHERE workflow_id=w.id AND active),(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind IN ('RUN_AGENT_TURN','PREPARE_AGENT_TURN') AND status IN ('AVAILABLE','LEASED')),a.infrastructure_failures FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id JOIN workflow_attempts a ON a.workflow_id=w.id AND a.active WHERE w.id=$1`, f.workflowID).Scan(&raw, &waiting, &active, &scheduled, &failures)
		if err != nil || active != 0 || failures != 0 || (turn != 3 && (!waiting || scheduled != 0)) || (turn == 3 && scheduled != 1) {
			t.Fatal("waiting retained execution or consumed retries", err, waiting, active, scheduled, failures)
		}
		var state collaboration.State
		if err = json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		if turn == 1 && (state.Planning != nil && len(state.Planning.Revisions) != 0) {
			t.Fatal("a full checkpoint interrupted the next question round")
		}
		if turn == 3 {
			continue
		} // late input already queued the sole successor
		command := collaboration.Command{Kind: "answer", Target: state.Rounds[len(state.Rounds)-1].Reference, Body: "Yes to both: target small teams without external accounts and keep selected checklist steps explicit."}
		if turn == 2 {
			command.Body = "yes; use ephemeral local status without retained data."
		}
		if turn == 1 {
			command.Body = "Change the proposed retention assumption: retain no personal onboarding data. Use ephemeral local status. Audience and explicit selected steps remain unchanged."
		}
		if turn == 4 {
			r := state.CurrentCheckpoint()
			if r == nil || !r.Published || r.Content.Status != "proposed" || len(r.Decisions) != 5 {
				t.Fatal("missing paired proposal or decisions", state.Planning)
			}
			command = collaboration.Command{Kind: "confirm-plan", Target: r.Reference}
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
	if len(state.ResolutionRejections) != 1 || state.ResolutionRejections[0].Validation.Code != "response_not_valid_for_question" {
		t.Fatal("missing durable resolution rejection", state.ResolutionRejections)
	}
	for _, round := range state.Rounds {
		for _, resolution := range round.Resolutions {
			if resolution.OriginOperationID == state.ResolutionRejections[0].OperationID {
				t.Fatal("rejection fabricated interpretation origin")
			}
		}
	}
	if state.Planning.ReadyRevision == "" || len(state.Planning.Revisions) != 1 || len(state.Planning.Revalidations) != 1 {
		t.Fatal("history missing", state.Planning)
	}
	if len(state.Planning.NonPublications) != 2 || state.Planning.NonPublications[0].Reason != "late_input" || state.Planning.NonPublications[1].Reason != "historical_decisions_unresolved" || !strings.Contains(state.Planning.Revisions[0].Content.RequirementsBrief, "without repository access") || !strings.Contains(state.Planning.Revisions[0].Content.TechnicalPlan, "standalone operator CLI") {
		t.Fatal("amendment did not change the actual plan", state.Planning)
	}
	latestAudience := ""
	for _, resolution := range state.Rounds[0].Resolutions {
		if resolution.QuestionID == "Q1" {
			latestAudience = resolution.Explanation
		}
	}
	if !strings.Contains(latestAudience, "without repository access") {
		t.Fatal("missing explicit reinterpretation", state.Rounds[0])
	}
	if len(state.Rounds) != 3 || len(state.Rounds[0].Resolutions) != 5 || len(state.Assessments) != 6 || len(state.Inputs) != 6 || len(state.Assessments[1].Corrects) != 1 || state.Assessments[1].Corrects[0] != state.Inputs[0].ID || state.Unresolved(participant) {
		t.Fatal("historical reconciliation lost provenance or left a fence", state)
	}
	var attempts, failures, reviews int
	if err = f.pool(t).QueryRow(f.ctx, `SELECT count(*),sum(infrastructure_failures),sum(CASE WHEN review_usage='{}'::jsonb THEN 0 ELSE 1 END) FROM workflow_attempts WHERE workflow_id=$1`, f.workflowID).Scan(&attempts, &failures, &reviews); err != nil || attempts != 1 || failures != 0 || reviews != 0 {
		t.Fatal("budgets changed", err, attempts, failures, reviews)
	}
	checkpointPosts := 0
	f.github.mutex.Lock()
	for _, comment := range f.github.comments {
		if strings.Contains(comment.Body, "Complete requirements brief") {
			checkpointPosts++
		}
	}
	f.github.mutex.Unlock()
	if checkpointPosts != 1 {
		t.Fatal("duplicate or missing checkpoint publication", checkpointPosts)
	}
	t.Log("PASS: controlled retained Planner rounds, changed assumption, paired draft/proposal, exact confirmation and final revalidation; no implementation. This does not assess reasoning quality.")
}
