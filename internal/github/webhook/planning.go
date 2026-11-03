package webhook

import (
	"context"
	"fmt"
	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type PlanningActivationHandler interface {
	AuthorizePlanningActivation(context.Context, store.WebhookClaim, *NormalizedEvent) (bool, error)
}

func (a *IssueCollaboration) AuthorizePlanningActivation(ctx context.Context, claim store.WebhookClaim, event *NormalizedEvent) (bool, error) {
	if event.EventName != "issues" || event.Action != "labeled" || event.Issue == nil || event.Label != "omnigrex:plan" && event.Label != "omnigrex:run" {
		return true, nil
	}
	source, ok := a.Store.(interface {
		GetPlanningActivationState(context.Context, int64, int64, int64) (store.PlanningActivationState, error)
	})
	if !ok {
		return event.Label != "omnigrex:plan", nil
	}
	state, err := source.GetPlanningActivationState(ctx, claim.RepositoryID, claim.IssueID, claim.IssueNumber)
	if err != nil {
		return false, err
	}
	if event.Label == "omnigrex:run" && (!state.PlanningOnly || state.State == "NEEDS_HUMAN") {
		return true, nil
	}
	credential, err := a.Credentials.RepositoryCredential(ctx, claim.RepositoryOwner, claim.RepositoryName)
	if err != nil {
		return false, err
	}
	feedback := ""
	if event.Label == "omnigrex:run" {
		feedback = "Planning does not authorize implementation. omnigrex:run cannot convert this planning Work Item, including after planning-ready; planning-to-development is follow-up #165."
	} else if state.Exists {
		feedback = "Planning requires an open, unstarted Work Item. Existing development or completed planning cannot be switched or restarted with omnigrex:plan."
	} else {
		if event.Sender == nil {
			return false, ErrMalformedPayload
		}
		allowed, err := a.API.CanDecideWorkItem(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, event.Sender.Login, event.Sender.ID)
		if err != nil {
			return false, err
		}
		if !allowed {
			feedback = "Planning activation requires current repository write, maintain or admin permission."
		} else {
			api, ok := a.API.(interface {
				GetIssue(context.Context, string, string, string, int) (githubapi.Issue, error)
			})
			if !ok {
				return false, fmt.Errorf("planning activation requires current Issue lookup")
			}
			issue, err := api.GetIssue(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, int(claim.IssueNumber))
			if err != nil {
				return false, err
			}
			if issue.ID != claim.IssueID || int64(issue.Number) != claim.IssueNumber || issue.State != "open" {
				feedback = "Planning requires the exact open Work Item Issue."
			} else {
				event.PlanningAuthorized = true
				event.PlanningRequest = &collaboration.OriginalRequest{Title: issue.Title, Body: issue.Body, Source: collaboration.Source{Channel: "github", ID: fmt.Sprint(issue.ID), URL: issue.HTMLURL}}
				return true, nil
			}
		}
	}
	// Reconcile delivery-marked rejection feedback before retrying an uncertain POST.
	body := feedback + "\n\n<!-- omnigrex-planning-activation:" + claim.DeliveryID + " -->"
	comments, err := a.API.ListIssueComments(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, int(claim.IssueNumber))
	if err != nil {
		return false, err
	}
	for _, c := range comments {
		if c.Body == body {
			return false, nil
		}
	}
	_, err = a.API.CreateIssueComment(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, int(claim.IssueNumber), githubapi.CommentRequest{Body: body})
	return false, err
}
