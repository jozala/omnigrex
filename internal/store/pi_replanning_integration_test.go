//go:build integration

package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func testPiReplanningWorkflow(t *testing.T, unchanged bool) {
	mode := "replanning-revised"
	if unchanged {
		mode = "replanning-unchanged"
	}
	f := newPiWorkflowFixture(t, mode)
	send := func(command collaboration.Command) {
		t.Helper()
		r, err := f.database.AdmitHumanInput(f.ctx, store.HumanInputSubmission{WorkflowID: f.workflowID, Command: command, Actor: collaboration.Actor{ID: "42", Name: "Maintainer"}, Source: collaboration.Source{Channel: "qualification", ID: uuid.NewString()}, Authorized: true, At: time.Now()})
		if err != nil || r.ResponseID == "" {
			t.Fatal(r, err)
		}
	}
	var planner, plannerSession, developer, developerSession, attempt, original string
	var proposalID int64
	humanEffort := 0
	revisedConfirmed := false
	for turn := 0; turn < 12; turn++ {
		prepared := f.prepare(t)
		p := prepared.Commit
		if attempt == "" {
			attempt = p.Turn.WorkflowAttemptID
		}
		if attempt != p.Turn.WorkflowAttemptID {
			t.Fatal("replanning created another Attempt")
		}
		switch p.Participant.Role {
		case workflow.RolePlanner:
			if planner == "" {
				planner = p.Participant.ID
				plannerSession = p.Session.ID
			}
			if planner != p.Participant.ID || plannerSession != p.Session.ID {
				t.Fatal("Planner continuity lost")
			}
		case workflow.RoleDeveloper:
			if developer == "" {
				developer = p.Participant.ID
				developerSession = p.Session.ID
			}
			if developer != p.Participant.ID || developerSession != p.Session.ID {
				t.Fatal("Developer continuity lost")
			}
		}
		if processed, err := f.worker.ProcessNext(f.ctx); err != nil || !processed {
			t.Fatal("replanning Turn", turn, processed, err)
		}
		var raw []byte
		var status string
		if err := f.pool(t).QueryRow(f.ctx, `SELECT c.state,w.status FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, f.workflowID).Scan(&raw, &status); err != nil {
			t.Fatal(err)
		}
		var s collaboration.State
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if status == string(workflow.StateNeedsHuman) {
			t.Fatalf("unexpected Human Handoff at Turn %d", turn)
		}
		if original == "" && s.Planning != nil {
			if r := s.CurrentCheckpoint(); r != nil {
				original = r.ID
			}
		}
		if status == string(workflow.StatePRReady) {
			var counter, usage, reviewCount int
			var ready, plan, accepted string
			var retainedID int64
			if err := f.pool(t).QueryRow(f.ctx, `SELECT a.autonomous_replanning_returns,(a.review_usage->>'review')::int,p.ready_for_sha,p.ready_for_plan_revision_id,p.pull_request_id,c.state->'planning'->'implementation'->>'accepted_revision_id',(SELECT count(*) FROM change_proposal_reviews WHERE change_proposal_id=p.id AND accepted) FROM workflows w JOIN workflow_attempts a ON a.workflow_id=w.id AND a.active JOIN change_proposals p ON p.workflow_id=w.id AND p.active JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, f.workflowID).Scan(&counter, &usage, &ready, &plan, &retainedID, &accepted, &reviewCount); err != nil {
				t.Fatal(err)
			}
			if counter != 1 || usage != 1 || reviewCount != 1 || plan != accepted || ready == "" || proposalID != retainedID || f.github.openCalls != 1 || len(s.Planning.Implementation.Bindings) != 2 {
				t.Fatal("lost revision, proposal or budget provenance", counter, usage, reviewCount, plan, accepted, proposalID, retainedID, s)
			}
			if unchanged && (len(s.Planning.Revisions) != 1 || accepted != original || humanEffort != 2) {
				t.Fatal("false alarm demanded new documents or consent", humanEffort, s)
			}
			if !unchanged && (len(s.Planning.Revisions) != 2 || accepted == original || humanEffort != 5) {
				t.Fatal("material return missed human decision or exact consent", humanEffort, s)
			}
			t.Logf("controlled evaluation: human commands=%d; autonomous returns=1; same Change Proposal; partial history preserved; avoidable rework limited to isolated restart model; executable restart/reset and identifier-exclusion checks, preserved-head ancestry, exact decision fidelity and final native review verified; not a real-model semantic-quality evaluation", humanEffort)
			return
		}
		if f.github.openCalls > 0 && proposalID == 0 {
			proposalID = f.github.pullRequest().ID
		}
		if !s.Waiting {
			continue
		}
		r := s.CurrentCheckpoint()
		if len(s.Rounds) > 0 && s.Unresolved(planner) {
			round := s.Rounds[len(s.Rounds)-1]
			body := "Use transient checklist state, without personal-data retention."
			if s.ActiveReplanning() != nil {
				body = "Retain nonpersonal checklist decisions across restart; never retain user identifiers. Reset deletes decisions."
			}
			send(collaboration.Command{Kind: "answer", Target: round.Reference, Body: body})
			humanEffort++
		} else if r != nil {
			if s.ActiveReplanning() != nil && !revisedConfirmed {
				send(collaboration.Command{Kind: "confirm-plan", Target: r.Reference})
				revisedConfirmed = true
			} else {
				send(collaboration.Command{Kind: "implement", Target: r.Reference})
			}
			humanEffort++
		} else {
			t.Fatal("unexpected wait without question/pair")
		}
	}
	t.Fatal("controlled replanning did not complete within bounded Turns")
}
