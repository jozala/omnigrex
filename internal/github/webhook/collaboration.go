package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type CollaborationHandler interface {
	HandleCollaboration(context.Context, store.WebhookClaim) (bool, error)
}
type CollaborationStore interface {
	FindCollaborationWorkItem(context.Context, int64, int64, int64) (string, error)
	AdmitHumanInput(context.Context, store.HumanInputSubmission) (collaboration.Receipt, error)
}
type CollaborationAPI interface {
	CanDecideWorkItem(context.Context, string, string, string, string, int64) (bool, error)
	ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error)
	CreateIssueComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error)
}
type CollaborationCredentials interface {
	RepositoryCredential(context.Context, string, string) (string, error)
}

// IssueCollaboration translates new Issue commands to the same application
// ingestion operation that another transport can call. It never prompts a Session.
type IssueCollaboration struct {
	Store       CollaborationStore
	API         CollaborationAPI
	Credentials CollaborationCredentials
}

func (adapter *IssueCollaboration) HandleCollaboration(ctx context.Context, claim store.WebhookClaim) (bool, error) {
	if claim.EventName != "issue_comment" {
		return false, nil
	}
	// Edits/deletions, including changes to an accepted command, are inert.
	if claim.Action != "created" {
		return true, nil
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Issue struct {
			ID          int64           `json:"id"`
			Number      int64           `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Sender struct {
			ID int64 `json:"id"`
		} `json:"sender"`
		Comment struct {
			ID        int64     `json:"id"`
			Body      string    `json:"body"`
			HTMLURL   string    `json:"html_url"`
			CreatedAt time.Time `json:"created_at"`
			User      struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
		} `json:"comment"`
	}
	if json.Unmarshal(claim.Payload, &payload) != nil || payload.Action != claim.Action {
		return true, ErrMalformedPayload
	}
	command, recognized := ParseCollaborationCommand(claim.EventName, claim.Action, payload.Comment.Body)
	if !recognized {
		return true, nil
	}
	if payload.Repository.ID != claim.RepositoryID || payload.Repository.Name != claim.RepositoryName || payload.Repository.Owner.Login != claim.RepositoryOwner || payload.Issue.ID != claim.IssueID || payload.Issue.Number != claim.IssueNumber || payload.Comment.ID <= 0 || payload.Comment.CreatedAt.IsZero() || payload.Comment.User.ID <= 0 || payload.Comment.User.Login == "" || payload.Sender.ID != payload.Comment.User.ID {
		return true, ErrMalformedPayload
	}
	if len(payload.Issue.PullRequest) > 0 && string(payload.Issue.PullRequest) != "null" {
		return true, nil
	}
	// Bot commands are never admitted, including a command copied into an App comment.
	if payload.Comment.User.Type != "User" {
		return true, nil
	}
	workflowID, err := adapter.Store.FindCollaborationWorkItem(ctx, claim.RepositoryID, claim.IssueID, claim.IssueNumber)
	if err != nil {
		return true, err
	}
	if workflowID == "" {
		return true, nil
	}
	credential, err := adapter.Credentials.RepositoryCredential(ctx, claim.RepositoryOwner, claim.RepositoryName)
	if err != nil {
		return true, err
	}
	authorized, err := adapter.API.CanDecideWorkItem(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, payload.Comment.User.Login, payload.Comment.User.ID)
	if err != nil {
		return true, err
	}
	receipt, err := adapter.Store.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: workflowID, Command: command, Actor: collaboration.Actor{ID: fmt.Sprint(payload.Comment.User.ID), Name: payload.Comment.User.Login}, Source: collaboration.Source{Channel: "github", ID: fmt.Sprint(payload.Comment.ID), URL: payload.Comment.HTMLURL}, Authorized: authorized, At: claim.ReceivedAt})
	if err != nil {
		return true, err
	}
	marker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: workflowID, OperationID: strings.TrimPrefix(receipt.ID, "A")})
	if err != nil {
		return true, err
	}
	// Acknowledgments are an independently reconciled visible receipt, not the
	// agent's interpretation. A retry after an uncertain POST first checks it.
	comments, err := adapter.API.ListIssueComments(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, int(claim.IssueNumber))
	if err != nil {
		return true, err
	}
	for _, comment := range comments {
		if comment.Body == githubapi.JoinBodyParts(receipt.Feedback, marker) {
			return true, nil
		}
	}
	_, err = adapter.API.CreateIssueComment(ctx, credential, claim.RepositoryOwner, claim.RepositoryName, int(claim.IssueNumber), githubapi.CommentRequest{Body: receipt.Feedback, Marker: marker})
	return true, err
}
