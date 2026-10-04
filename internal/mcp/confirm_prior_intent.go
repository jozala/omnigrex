package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// confirmPriorTerminalIntent records the agent's fresh decision while using
// only the old, fenced MCP result. It never submits a second GitHub review.
func (backend *ProductionBackend) confirmPriorTerminalIntent(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	if backend.priorIntents == nil {
		return nil, ErrToolDependency
	}
	var arguments struct {
		OperationID        string `json:"operation_id"`
		SourceInvocationID string `json:"source_invocation_id"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil ||
		arguments.OperationID == "" || arguments.SourceInvocationID == "" {
		return nil, ErrInvalidInvocation
	}
	source, err := backend.priorIntents.GetConfirmablePriorTerminalIntent(ctx, invocation.lease, arguments.SourceInvocationID)
	if err != nil {
		if errors.Is(err, store.ErrMutationOperationConflict) || errors.Is(err, store.ErrAgentTurnFenceLost) {
			return nil, ErrToolPrecondition
		}
		return nil, ErrToolDependency
	}
	if source.ID != arguments.SourceInvocationID || source.ExpectedSHA == "" ||
		source.ExpectedSHA != backend.currentPublishedHead(invocation.Scope) {
		return nil, ErrToolPrecondition
	}
	credential, err := backend.credential(ctx, ToolConfirmPriorTerminalIntent, invocation.Scope.Role, invocation.Scope.Repository)
	if err != nil {
		return nil, ErrToolDependency
	}
	var number int64
	var expectedPullRequestID int64
	var reviewID int64
	var reviewNode, reviewState, reviewCommit string
	var reviewActor int64
	switch invocation.Scope.Role {
	case workflow.RoleDeveloper:
		if source.ToolName != ToolRequestReview || (source.ExternalService != "omnigrex" && source.ExternalService != "github") ||
			source.ExternalResourceID != fmt.Sprintf("%d:%s", invocation.Scope.Repository.ID, invocation.Scope.Branch) {
			return nil, ErrToolPrecondition
		}
		var result struct {
			Outcome           string `json:"outcome"`
			PullRequestID     int64  `json:"pull_request_id"`
			PullRequestNumber int64  `json:"pull_request_number"`
			HeadSHA           string `json:"head_sha"`
		}
		if !decodeExactResult(source.Result, &result) || result.Outcome != "REVIEW_REQUESTED" ||
			result.PullRequestID <= 0 || result.PullRequestNumber <= 0 || result.HeadSHA != source.ExpectedSHA {
			return nil, ErrToolPrecondition
		}
		number, expectedPullRequestID = result.PullRequestNumber, result.PullRequestID
		knownPR := backend.currentPullRequest(invocation.Scope)
		if knownPR != nil && (knownPR.ID != result.PullRequestID || knownPR.Number != number) {
			return nil, ErrToolPrecondition
		}
	case workflow.RoleReviewer:
		if source.ToolName != ToolSubmitReview || source.ExternalService != "github" || invocation.Scope.PullRequest == nil ||
			source.ExternalResourceID != fmt.Sprintf("%d:%d", invocation.Scope.Repository.ID, invocation.Scope.PullRequest.ID) {
			return nil, ErrToolPrecondition
		}
		var result struct {
			ReviewID int64  `json:"review_id"`
			NodeID   string `json:"node_id"`
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			ActorID  int64  `json:"actor_id"`
			HTMLURL  string `json:"html_url"`
		}
		if !decodeExactResult(source.Result, &result) || result.ReviewID <= 0 || result.NodeID == "" ||
			result.ActorID <= 0 || result.CommitID != source.ExpectedSHA ||
			(result.State != "APPROVED" && result.State != "CHANGES_REQUESTED") {
			return nil, ErrToolPrecondition
		}
		reviewID, reviewNode, reviewState, reviewCommit, reviewActor = result.ReviewID, result.NodeID, result.State, result.CommitID, result.ActorID
		number = invocation.Scope.PullRequest.Number
		expectedPullRequestID = invocation.Scope.PullRequest.ID
	default:
		return nil, ErrToolNotAuthorized
	}
	current, err := backend.github.GetPullRequest(ctx, credential, invocation.Scope.Repository.Owner,
		invocation.Scope.Repository.Name, int(number))
	if err != nil {
		return nil, githubObservationFailure(err)
	}
	if current.State != "open" || current.Merged || current.ID != expectedPullRequestID || current.Number != int(number) ||
		current.Head.Ref != invocation.Scope.Branch || current.Head.SHA != source.ExpectedSHA ||
		current.Base.Ref != invocation.Scope.DefaultBranch ||
		invocation.Scope.PullRequest != nil && current.ID != invocation.Scope.PullRequest.ID {
		return nil, ErrToolPrecondition
	}
	if invocation.Scope.Role == workflow.RoleReviewer {
		reviews, err := backend.github.ListPullRequestReviews(ctx, credential, invocation.Scope.Repository.Owner,
			invocation.Scope.Repository.Name, int(number))
		if err != nil {
			return nil, githubObservationFailure(err)
		}
		matched, seenID := 0, false
		for _, review := range reviews {
			if review.ID == reviewID {
				seenID = true
			}
			if review.ID == reviewID && review.NodeID == reviewNode && review.State == reviewState &&
				review.CommitID == reviewCommit && review.User.ID == reviewActor {
				matched++
			}
		}
		// GitHub may not list an already-submitted review immediately. Record
		// the agent's confirmation of its durable source intent; terminal
		// reconciliation will still require the review to become visible.
		if seenID && matched != 1 {
			return nil, ErrToolPrecondition
		}
	}
	return json.Marshal(struct {
		SourceInvocationID string          `json:"source_invocation_id"`
		SourceTool         string          `json:"source_tool"`
		SourceOperationID  string          `json:"source_operation_id"`
		SourceRequest      json.RawMessage `json:"source_request"`
		SourceResult       json.RawMessage `json:"source_result"`
		ExternalService    string          `json:"external_service"`
		ExternalResourceID string          `json:"external_resource_id"`
		ExpectedSHA        string          `json:"expected_sha"`
	}{source.ID, source.ToolName, source.OperationID, source.Request, source.Result,
		source.ExternalService, source.ExternalResourceID, source.ExpectedSHA})
}
