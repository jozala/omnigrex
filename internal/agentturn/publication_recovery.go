package agentturn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var ErrPublicationConflict = errors.New("in-progress publication conflicts with Git or GitHub")

type PublicationRecoveryStore interface {
	ListParticipantPublicationMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error)
	BindAgentTurnPublication(context.Context, store.AgentTurnLease, store.AgentTurnPublication) error
}

type PublicationRecoveryRemote interface {
	ObserveRemoteBranch(context.Context, string, string, string) (string, error)
	ReconcilePublication(context.Context, workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error)
	PrepareRecoveredPublication(context.Context, string, string, string, string) error
}

type PublicationRecoveryGitHub interface {
	ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error)
}

type ExecutionPublicationRecovery interface {
	Recover(context.Context, store.AgentTurnLease, store.AgentTurnExecutionContext, string, string, string) error
}

// PublicationRecovery binds the exact publications of a prior turn by the same
// Participant, without turning an open Pull Request into a review request.
type PublicationRecovery struct {
	store       PublicationRecoveryStore
	remote      PublicationRecoveryRemote
	github      PublicationRecoveryGitHub
	credentials RepositoryCredentialProvider
}

// NewPublicationRecoveryWithCredentials refreshes the repository credential
// after the recovered publication is prepared: the post-clone PR and branch
// observations that follow unbounded Git I/O must not reuse a token that
// expired during the clone. A nil provider preserves the legacy behavior of
// reusing the passed credential throughout.
func NewPublicationRecoveryWithCredentials(database PublicationRecoveryStore, remote PublicationRecoveryRemote, github PublicationRecoveryGitHub, credentials RepositoryCredentialProvider) *PublicationRecovery {
	return &PublicationRecovery{store: database, remote: remote, github: github, credentials: credentials}
}

func (recovery *PublicationRecovery) Recover(ctx context.Context, lease store.AgentTurnLease, execution store.AgentTurnExecutionContext, repositoryURL, credential, defaultBranch string) error {
	if execution.ChangeProposal != nil || execution.Assignment.Role != workflow.RoleDeveloper {
		return nil
	}
	mutations, err := recovery.store.ListParticipantPublicationMutations(ctx, lease)
	if err != nil {
		return errors.New("read earlier publication evidence")
	}
	branch := fmt.Sprintf("omnigrex/issue-%d", execution.Issue.Number)
	actual, err := recovery.remote.ObserveRemoteBranch(ctx, repositoryURL, credential, branch)
	if err != nil {
		return errors.New("verify publication branch unavailable")
	}
	var publication store.AgentTurnPublication
	publication.HeadRef, publication.BaseRef = branch, defaultBranch
	var latest store.MutationReservation
	var opened store.MutationReservation
	openedURL := ""
	for _, mutation := range mutations {
		if mutation.State != store.MutationSucceeded {
			continue
		}
		switch mutation.ToolName {
		case mcp.ToolPublishChanges:
			var result struct {
				Head    string `json:"head"`
				Branch  string `json:"branch"`
				Changed bool   `json:"changed"`
			}
			if mutation.ExternalService != "git" || mutation.ExternalResourceID != fmt.Sprintf("%d:%s", execution.Repository.ID, branch) ||
				!decodePublicationObject(mutation.Result, &result) || !validPublicationSHA(result.Head) || result.Branch != branch ||
				latest.ID != "" && mutation.ExpectedSHA != publication.HeadSHA {
				return ErrPublicationConflict
			}
			if !result.Changed {
				if result.Head != mutation.ExpectedSHA {
					return ErrPublicationConflict
				}
				continue
			}
			if result.Head == mutation.ExpectedSHA {
				return ErrPublicationConflict
			}
			if mutation.HistoryPublication && mutation.ProposedSHA != result.Head {
				return ErrPublicationConflict
			}
			publication.HeadSHA, publication.SourcePublishMutationID = result.Head, mutation.ID
			latest = mutation
		case mcp.ToolOpenPR:
			var result struct {
				ID      int64  `json:"pull_request_id"`
				NodeID  string `json:"node_id"`
				Number  int64  `json:"number"`
				HTMLURL string `json:"html_url"`
				HeadSHA string `json:"head_sha"`
			}
			if opened.ID != "" || mutation.ExternalService != "github" ||
				mutation.ExternalResourceID != fmt.Sprintf("%d:%s:%s", execution.Repository.ID, branch, defaultBranch) ||
				!decodePublicationObject(mutation.Result, &result) || result.ID <= 0 || result.Number <= 0 ||
				result.NodeID == "" || result.HTMLURL == "" || !validPublicationSHA(result.HeadSHA) || result.HeadSHA != mutation.ExpectedSHA ||
				latest.ID == "" || publication.HeadSHA != mutation.ExpectedSHA {
				return ErrPublicationConflict
			}
			opened = mutation
			openedURL = result.HTMLURL
			publication.PullRequestID, publication.PullRequestNumber, publication.PullRequestNodeID = result.ID, result.Number, result.NodeID
			publication.SourceOpenPRMutationID = mutation.ID
		}
	}
	if latest.ID == "" {
		if actual != "" || opened.ID != "" {
			return ErrPublicationConflict
		}
		return nil
	}
	if actual != publication.HeadSHA {
		return ErrPublicationConflict
	}
	observed, err := recovery.remote.ReconcilePublication(ctx, workspace.PublicationReconciliation{
		AssignmentID: execution.Assignment.ID, RepositoryURL: repositoryURL, Credential: credential,
		BaseRevision: latest.ExpectedSHA, ProposedRevision: latest.ProposedSHA, HistoryPublication: latest.HistoryPublication,
		Branch: branch, OperationID: latest.ID,
	})
	if err != nil {
		return errors.New("verify publication commit unavailable")
	}
	if observed.Outcome != workspace.PublicationReconciliationFound || observed.Head != publication.HeadSHA {
		return ErrPublicationConflict
	}
	if err := recovery.verifyPullRequest(ctx, execution, credential, publication, opened, openedURL); err != nil {
		return err
	}
	if err := recovery.remote.PrepareRecoveredPublication(ctx, execution.Assignment.ID, repositoryURL, credential, publication.HeadSHA); err != nil {
		return errors.New("prepare recovered publication unavailable")
	}
	if !nilDependency(recovery.credentials) {
		// The clone above performs unbounded Git I/O; re-acquire a current
		// credential for the observations that follow instead of reusing a
		// token that may have expired during preparation.
		fresh, err := recovery.credentials.RepositoryCredential(ctx, execution.Repository.Owner, execution.Repository.Name)
		if err != nil || strings.TrimSpace(fresh) == "" {
			return errors.New("verify publication Pull Requests unavailable")
		}
		credential = fresh
	}
	// Git's publication reconciliation can find an older operation in the
	// current head's ancestry. Reobserve the exact ref after every clone and PR
	// read, before binding a successor Turn to that head.
	if err := recovery.verifyPullRequest(ctx, execution, credential, publication, opened, openedURL); err != nil {
		return err
	}
	current, err := recovery.remote.ObserveRemoteBranch(ctx, repositoryURL, credential, branch)
	if err != nil {
		return errors.New("verify publication branch unavailable")
	}
	if current != publication.HeadSHA {
		return ErrPublicationConflict
	}
	if err := recovery.store.BindAgentTurnPublication(ctx, lease, publication); err != nil {
		return errors.New("bind in-progress publication unavailable")
	}
	return nil
}

func (recovery *PublicationRecovery) verifyPullRequest(ctx context.Context, execution store.AgentTurnExecutionContext, credential string, publication store.AgentTurnPublication, opened store.MutationReservation, openedURL string) error {
	pullRequests, err := recovery.github.ListPullRequests(ctx, credential, execution.Repository.Owner, execution.Repository.Name,
		githubapi.ListPullRequestsRequest{Head: publication.HeadRef})
	if err != nil {
		return errors.New("verify publication Pull Requests unavailable")
	}
	var current *githubapi.PullRequest
	for index := range pullRequests {
		if pullRequests[index].State != "open" {
			continue
		}
		if current != nil || opened.ID == "" {
			return ErrPublicationConflict
		}
		current = &pullRequests[index]
	}
	if opened.ID == "" {
		return nil
	}
	if current == nil {
		return ErrPublicationConflict
	}
	var request struct {
		OperationID string `json:"operation_id"`
		Title       string `json:"title"`
		Body        string `json:"body"`
	}
	marker, markerErr := githubapi.RenderMarker(githubapi.Marker{
		WorkflowID: execution.WorkflowID, AgentAssignmentID: execution.Assignment.ID, OperationID: opened.ID,
	})
	if markerErr != nil || !decodePublicationObject(opened.Request, &request) || request.OperationID != opened.OperationID ||
		current.ID != publication.PullRequestID || int64(current.Number) != publication.PullRequestNumber ||
		current.NodeID != publication.PullRequestNodeID || current.HTMLURL != openedURL ||
		current.Head.Ref != publication.HeadRef || current.Head.SHA != publication.HeadSHA ||
		current.Base.Ref != publication.BaseRef || current.Title != request.Title ||
		current.Body != githubapi.JoinBodyParts(request.Body, fmt.Sprintf("Closes #%d", execution.Issue.Number), marker) {
		return ErrPublicationConflict
	}
	return nil
}

func decodePublicationObject(raw json.RawMessage, destination any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func validPublicationSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, character := range sha {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
