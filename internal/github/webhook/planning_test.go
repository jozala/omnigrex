package webhook

import (
	"context"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"strings"
	"testing"
)

type planningActivationFixture struct {
	*collaborationFixture
	routing store.PlanningActivationState
	issue   githubapi.Issue
	checked int
}

func (f *planningActivationFixture) GetPlanningActivationState(context.Context, int64, int64, int64) (store.PlanningActivationState, error) {
	return f.routing, nil
}
func (f *planningActivationFixture) GetIssue(context.Context, string, string, string, int) (githubapi.Issue, error) {
	return f.issue, nil
}
func (f *planningActivationFixture) CanDecideWorkItem(context.Context, string, string, string, string, int64) (bool, error) {
	f.checked++
	return f.authorized, nil
}
func TestPlanningActivationCurrentAuthorityAndOriginal(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		f := &planningActivationFixture{collaborationFixture: &collaborationFixture{authorized: authorized}, issue: githubapi.Issue{ID: 2, Number: 3, Title: "Rough idea", Body: "Original request\nwith lines", State: "open", HTMLURL: "https://example.invalid/issue/3"}}
		a := &IssueCollaboration{Store: f, API: f, Credentials: f}
		claim := collaborationClaim("created", "")
		claim.EventName = "issues"
		claim.Action = "labeled"
		claim.DeliveryID = "11111111-1111-4111-8111-111111111111"
		claim.IssueID = 2
		claim.IssueNumber = 3
		event := &NormalizedEvent{EventName: "issues", Action: "labeled", Label: "omnigrex:plan", Issue: &Issue{ID: 2, Number: 3}, Sender: &Actor{ID: 42, Login: "maintainer"}}
		allowed, err := a.AuthorizePlanningActivation(context.Background(), claim, event)
		if err != nil || f.checked != 1 {
			t.Fatal(err)
		}
		if authorized {
			if !allowed || !event.PlanningAuthorized || event.PlanningRequest == nil || event.PlanningRequest.Body != f.issue.Body || event.PlanningRequest.Source.URL != f.issue.HTMLURL {
				t.Fatal(event)
			}
		} else if allowed || event.PlanningAuthorized || event.PlanningRequest != nil || len(f.comments) != 1 {
			t.Fatal("unauthorized activation", event)
		}
	}
}
func TestPlanningRunRejectionReconcilesLostReceipt(t *testing.T) {
	for _, state := range []string{"PLANNING", "PLAN_READY", "CLOSED"} {
		f := &planningActivationFixture{collaborationFixture: &collaborationFixture{authorized: true, loseResponse: true}, routing: store.PlanningActivationState{Exists: true, PlanningOnly: true, State: state}}
		a := &IssueCollaboration{Store: f, API: f, Credentials: f}
		claim := collaborationClaim("created", "")
		claim.EventName = "issues"
		claim.Action = "labeled"
		claim.DeliveryID = "11111111-1111-4111-8111-111111111111"
		event := &NormalizedEvent{EventName: "issues", Action: "labeled", Label: "omnigrex:run", Issue: &Issue{ID: 2, Number: 3}, Sender: &Actor{ID: 42, Login: "maintainer"}}
		if allowed, err := a.AuthorizePlanningActivation(context.Background(), claim, event); allowed || err == nil {
			t.Fatal(allowed, err)
		}
		allowed, err := a.AuthorizePlanningActivation(context.Background(), claim, event)
		if allowed || err != nil || len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "#165") {
			t.Fatal(allowed, err, f.comments)
		}
	}
}
func TestConfirmPlanOnlyNewCommentFirstNonblankLine(t *testing.T) {
	for _, body := range []string{"> /omnigrex confirm-plan P1", "Text\n/omnigrex confirm-plan P1", "    /omnigrex confirm-plan P1"} {
		if _, ok := ParseCollaborationCommand("issue_comment", "created", body); ok {
			t.Fatal(body)
		}
	}
	c, ok := ParseCollaborationCommand("issue_comment", "created", "\n/omnigrex confirm-plan P7\n")
	if !ok || c.Kind != "confirm-plan" || c.Target != "P7" {
		t.Fatal(c)
	}
	for _, action := range []string{"edited", "deleted"} {
		if _, ok = ParseCollaborationCommand("issue_comment", action, "/omnigrex confirm-plan P7"); ok {
			t.Fatal(action)
		}
	}
}
