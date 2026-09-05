package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/gitremote"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var (
	ErrInvalidReconcilerConfiguration   = errors.New("invalid production mutation reconciler configuration")
	ErrInvalidMutationReconciliation    = errors.New("invalid mutation reconciliation")
	ErrMutationReconciliationDependency = errors.New("mutation reconciliation dependency failed")
)

// PublicationReconciler is the read-only publication artifact seam used during recovery.
type PublicationReconciler interface {
	ReconcilePublication(context.Context, workspace.PublicationReconciliation) (workspace.PublicationReconciliationResult, error)
}

// MutationReconciliationDisposition classifies what durable external reads prove.
type MutationReconciliationDisposition string

const (
	ReconciliationFound            MutationReconciliationDisposition = "FOUND"
	ReconciliationDefinitelyFailed MutationReconciliationDisposition = "DEFINITELY_FAILED"
	ReconciliationUnresolved       MutationReconciliationDisposition = "UNRESOLVED"
)

// MutationReconciliationResult contains a terminal Store outcome when one was proved.
type MutationReconciliationResult struct {
	Disposition MutationReconciliationDisposition
	Outcome     store.RecoveredMutationOutcome
}

// MutationReconciler resolves one durable reservation without repeating its mutation request.
type MutationReconciler interface {
	Reconcile(context.Context, store.AgentTurnMutationReconciliationContext, store.MutationReservation) (MutationReconciliationResult, error)
}

type ProductionReconcilerConfig struct {
	GitHub           GitHubAPI
	Credentials      RepositoryCredentials
	Publications     PublicationReconciler
	GitRemoteBaseURL string
}

// ProductionReconciler finds exact GitHub and Git publication artifacts from durable reservation data.
type ProductionReconciler struct {
	github       GitHubAPI
	credentials  RepositoryCredentials
	publications PublicationReconciler
	remoteBase   gitremote.BaseURL
}

var (
	_ MutationReconciler    = (*ProductionReconciler)(nil)
	_ PublicationReconciler = (*workspace.Lifecycle)(nil)
)

func NewProductionReconciler(config ProductionReconcilerConfig) (*ProductionReconciler, error) {
	if interfaceNil(config.GitHub) || interfaceNil(config.Credentials) || interfaceNil(config.Publications) {
		return nil, ErrInvalidReconcilerConfiguration
	}
	remoteBase, err := gitremote.ParseBaseURL(config.GitRemoteBaseURL)
	if err != nil {
		return nil, ErrInvalidReconcilerConfiguration
	}
	return &ProductionReconciler{github: config.GitHub, credentials: config.Credentials, publications: config.Publications, remoteBase: remoteBase}, nil
}

func (reconciler *ProductionReconciler) Reconcile(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	if !validReconciliationScope(reconciliation, mutation) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	callerOperationID, err := requiredStringArgument(mutation.Request, "operation_id")
	if err != nil || callerOperationID != mutation.OperationID {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	marker, err := githubapi.RenderMarker(githubapi.Marker{
		WorkflowID: reconciliation.WorkflowID, AgentAssignmentID: reconciliation.Turn.AgentAssignmentID, OperationID: mutation.ID,
	})
	if err != nil {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}

	switch mutation.ToolName {
	case ToolPublishChanges:
		return reconciler.reconcilePublication(ctx, reconciliation, mutation)
	case ToolOpenPR:
		return reconciler.reconcileOpenPullRequest(ctx, reconciliation, mutation, marker)
	case ToolCommentOnIssue:
		return reconciler.reconcileComment(ctx, reconciliation, mutation, marker, false)
	case ToolCommentOnPullRequest:
		return reconciler.reconcileComment(ctx, reconciliation, mutation, marker, true)
	case ToolSubmitReview:
		return reconciler.reconcileReview(ctx, reconciliation, mutation, marker)
	case ToolRequestReview:
		return reconciler.reconcileRequestReview(ctx, reconciliation, mutation)
	case ToolReportBlocked:
		return reconcileReportBlocked(reconciliation, mutation)
	default:
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
}

func (reconciler *ProductionReconciler) reconcilePublication(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	branch, ok := exactBranchResource(mutation.ExternalResourceID, reconciliation.Repository.ID)
	if !ok || reconciliation.Role != workflow.RoleDeveloper || mutation.ExternalService != "git" || !validRevision(mutation.ExpectedSHA) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	credential, err := reconciler.developerCredential(ctx, reconciliation.Repository)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	expectedOldHead := ""
	if proposal := reconciliation.ChangeProposal; proposal != nil && proposal.HeadRef == branch && exactTurnExpectedHead(reconciliation, mutation) {
		expectedOldHead = mutation.ExpectedSHA
	}
	repositoryURL, err := reconciler.remoteBase.RepositoryURL(reconciliation.Repository.Owner, reconciliation.Repository.Name)
	if err != nil {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	observed, err := reconciler.publications.ReconcilePublication(ctx, workspace.PublicationReconciliation{
		AssignmentID:  reconciliation.Turn.AgentAssignmentID,
		RepositoryURL: repositoryURL,
		Credential:    credential, BaseRevision: mutation.ExpectedSHA, ExpectedOldHead: expectedOldHead,
		Branch: branch, OperationID: mutation.ID,
	})
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("reconcile publication")
	}
	switch observed.Outcome {
	case workspace.PublicationReconciliationFound:
		if !validRevision(observed.Head) || observed.Head == mutation.ExpectedSHA {
			return unresolvedReconciliation(), nil
		}
		result, err := json.Marshal(struct {
			Head    string `json:"head"`
			Branch  string `json:"branch"`
			Changed bool   `json:"changed"`
		}{observed.Head, branch, true})
		if err != nil {
			return MutationReconciliationResult{}, dependencyError("encode publication result")
		}
		return foundReconciliation(result), nil
	case workspace.PublicationReconciliationAbsent:
		if observed.Head != "" && observed.Head != mutation.ExpectedSHA {
			return unresolvedReconciliation(), nil
		}
		return unresolvedReconciliation(), nil
	case workspace.PublicationReconciliationUnknown:
		return unresolvedReconciliation(), nil
	default:
		return unresolvedReconciliation(), nil
	}
}

func (reconciler *ProductionReconciler) reconcileOpenPullRequest(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation, marker string) (MutationReconciliationResult, error) {
	branch, base, ok := exactOpenPullRequestResource(mutation.ExternalResourceID, reconciliation.Repository.ID)
	if !ok || reconciliation.Role != workflow.RoleDeveloper || mutation.ExternalService != "github" || !exactTurnExpectedHead(reconciliation, mutation) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !decodePersistedRequest(mutation.Request, &request) || strings.TrimSpace(request.Title) == "" {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	if proposal := reconciliation.ChangeProposal; proposal != nil &&
		(proposal.HeadRef != branch || proposal.BaseRef != base) {
		return unresolvedReconciliation(), nil
	}
	credential, err := reconciler.developerCredential(ctx, reconciliation.Repository)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	pullRequests, err := reconciler.github.ListPullRequests(ctx, credential, reconciliation.Repository.Owner, reconciliation.Repository.Name, githubapi.ListPullRequestsRequest{Head: branch, Base: base})
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("list Pull Requests")
	}
	expectedBody := joinVisibleParts(request.Body, fmt.Sprintf("Closes #%d", reconciliation.Issue.Number), marker)
	matches := make([]githubapi.PullRequest, 0, 1)
	conflict := false
	for _, pullRequest := range pullRequests {
		marked := hasExactOperationMarker(pullRequest.Body, reconciliation.WorkflowID, reconciliation.Turn.AgentAssignmentID, mutation.ID)
		if !marked {
			continue
		}
		if pullRequest.Head.Ref != branch || pullRequest.Title != request.Title || pullRequest.Body != expectedBody ||
			pullRequest.Base.Ref != base || reconciliation.ChangeProposal != nil &&
			(pullRequest.ID != reconciliation.ChangeProposal.PullRequestID || int64(pullRequest.Number) != reconciliation.ChangeProposal.PullRequestNumber) {
			conflict = true
			continue
		}
		matches = append(matches, pullRequest)
	}
	if len(matches) == 1 && !conflict {
		matched := matches[0]
		matched.Head.SHA = mutation.ExpectedSHA
		result, err := encodePullRequestResult(matched)
		if err != nil {
			return unresolvedReconciliation(), nil
		}
		return foundReconciliation(result), nil
	}
	if len(matches) == 0 && !conflict {
		return unresolvedReconciliation(), nil
	}
	return unresolvedReconciliation(), nil
}

func (reconciler *ProductionReconciler) reconcileComment(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation, marker string, pullRequestComment bool) (MutationReconciliationResult, error) {
	var request struct {
		Body string `json:"body"`
	}
	if mutation.ExternalService != "github" || !decodePersistedRequest(mutation.Request, &request) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	expectedBody := joinVisibleParts(request.Body, marker)
	number := int(reconciliation.Issue.Number)
	expectedResourceID := reconciliation.Issue.ID
	if pullRequestComment {
		if reconciliation.ChangeProposal == nil || !exactTurnExpectedHead(reconciliation, mutation) {
			return unresolvedReconciliation(), nil
		}
		number = int(reconciliation.ChangeProposal.PullRequestNumber)
		expectedResourceID = reconciliation.ChangeProposal.PullRequestID
	}
	if !exactNumericResource(mutation.ExternalResourceID, reconciliation.Repository.ID, expectedResourceID) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	credential, err := reconciler.developerCredential(ctx, reconciliation.Repository)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	comments, err := reconciler.github.ListIssueComments(ctx, credential, reconciliation.Repository.Owner, reconciliation.Repository.Name, number)
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("list comments")
	}
	matches := make([]githubapi.IssueComment, 0, 1)
	conflict := false
	for _, comment := range comments {
		if !hasExactOperationMarker(comment.Body, reconciliation.WorkflowID, reconciliation.Turn.AgentAssignmentID, mutation.ID) {
			continue
		}
		if comment.Body != expectedBody {
			conflict = true
			continue
		}
		matches = append(matches, comment)
	}
	if len(matches) == 1 && !conflict {
		result, err := encodeCommentResult(matches[0])
		if err != nil {
			return unresolvedReconciliation(), nil
		}
		return foundReconciliation(result), nil
	}
	if len(matches) == 0 && !conflict {
		return unresolvedReconciliation(), nil
	}
	return unresolvedReconciliation(), nil
}

func (reconciler *ProductionReconciler) reconcileReview(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation, marker string) (MutationReconciliationResult, error) {
	if reconciliation.Role != workflow.RoleReviewer || reconciliation.ChangeProposal == nil || mutation.ExternalService != "github" ||
		!exactNumericResource(mutation.ExternalResourceID, reconciliation.Repository.ID, reconciliation.ChangeProposal.PullRequestID) ||
		!exactTurnExpectedHead(reconciliation, mutation) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	var request struct {
		Event githubapi.ReviewEvent `json:"event"`
		Body  string                `json:"body"`
	}
	if !decodePersistedRequest(mutation.Request, &request) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	expectedState := "APPROVED"
	if request.Event == githubapi.ReviewRequestChanges {
		expectedState = "CHANGES_REQUESTED"
	} else if request.Event != githubapi.ReviewApprove {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	expectedBody, err := githubapi.EnsureMarker(request.Body, githubapi.Marker{
		WorkflowID: reconciliation.WorkflowID, AgentAssignmentID: reconciliation.Turn.AgentAssignmentID, OperationID: mutation.ID,
	})
	if err != nil || !strings.Contains(expectedBody, marker) {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	credential, err := reconciler.reviewerCredential(ctx, reconciliation.Repository)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	reviews, err := reconciler.github.ListPullRequestReviews(ctx, credential, reconciliation.Repository.Owner, reconciliation.Repository.Name, int(reconciliation.ChangeProposal.PullRequestNumber))
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("list Pull Request reviews")
	}
	matches := make([]githubapi.Review, 0, 1)
	conflict := false
	for _, review := range reviews {
		if !hasExactOperationMarker(review.Body, reconciliation.WorkflowID, reconciliation.Turn.AgentAssignmentID, mutation.ID) {
			continue
		}
		if review.Body != expectedBody || review.CommitID != mutation.ExpectedSHA || review.State != expectedState || review.User.ID <= 0 ||
			reconciliation.ReviewerActorID != 0 && review.User.ID != reconciliation.ReviewerActorID {
			conflict = true
			continue
		}
		matches = append(matches, review)
	}
	if len(matches) == 1 && !conflict {
		result, err := encodeReviewResult(matches[0])
		if err != nil {
			return unresolvedReconciliation(), nil
		}
		return foundReconciliation(result), nil
	}
	if len(matches) == 0 && !conflict {
		return unresolvedReconciliation(), nil
	}
	return unresolvedReconciliation(), nil
}

func (reconciler *ProductionReconciler) reconcileRequestReview(ctx context.Context, reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	var request struct {
		Summary string `json:"summary"`
	}
	if reconciliation.Role != workflow.RoleDeveloper || mutation.ExternalService != "omnigrex" || !decodePersistedRequest(mutation.Request, &request) || strings.TrimSpace(request.Summary) == "" {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	branch, resourceOK := exactBranchResource(mutation.ExternalResourceID, reconciliation.Repository.ID)
	if !resourceOK || !exactTurnExpectedHead(reconciliation, mutation) {
		return unresolvedReconciliation(), nil
	}
	if proposal := reconciliation.ChangeProposal; proposal != nil {
		if proposal.HeadRef != branch {
			return unresolvedReconciliation(), nil
		}
		return foundReviewRequest(proposal.PullRequestID, proposal.PullRequestNumber, mutation.ExpectedSHA)
	}

	credential, err := reconciler.developerCredential(ctx, reconciliation.Repository)
	if err != nil {
		return MutationReconciliationResult{}, err
	}
	pullRequests, err := reconciler.github.ListPullRequests(ctx, credential, reconciliation.Repository.Owner, reconciliation.Repository.Name, githubapi.ListPullRequestsRequest{Head: branch})
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("discover review Pull Request")
	}
	matches := make([]githubapi.PullRequest, 0, 1)
	for _, pullRequest := range pullRequests {
		if pullRequest.Head.Ref == branch &&
			hasAssignmentMarker(pullRequest.Body, reconciliation.WorkflowID, reconciliation.Turn.AgentAssignmentID) &&
			strings.Contains(pullRequest.Body, fmt.Sprintf("Closes #%d", reconciliation.Issue.Number)) {
			matches = append(matches, pullRequest)
		}
	}
	if len(matches) != 1 {
		return unresolvedReconciliation(), nil
	}
	return foundReviewRequest(matches[0].ID, int64(matches[0].Number), mutation.ExpectedSHA)
}

func reconcileReportBlocked(reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) (MutationReconciliationResult, error) {
	if mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != reconciliation.WorkflowID || mutation.ExpectedSHA != "" {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	var request struct {
		Reason  string `json:"reason"`
		Details string `json:"details"`
	}
	if !decodePersistedRequest(mutation.Request, &request) || strings.TrimSpace(request.Reason) == "" {
		return MutationReconciliationResult{}, ErrInvalidMutationReconciliation
	}
	result, err := json.Marshal(struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Details string `json:"details,omitempty"`
	}{"BLOCKED", request.Reason, request.Details})
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("encode blocked result")
	}
	return foundReconciliation(result), nil
}

func foundReviewRequest(pullRequestID, pullRequestNumber int64, head string) (MutationReconciliationResult, error) {
	if pullRequestID <= 0 || pullRequestNumber <= 0 {
		return unresolvedReconciliation(), nil
	}
	result, err := json.Marshal(struct {
		Outcome           string `json:"outcome"`
		PullRequestID     int64  `json:"pull_request_id"`
		PullRequestNumber int64  `json:"pull_request_number"`
		HeadSHA           string `json:"head_sha"`
	}{"REVIEW_REQUESTED", pullRequestID, pullRequestNumber, head})
	if err != nil {
		return MutationReconciliationResult{}, dependencyError("encode review request result")
	}
	return foundReconciliation(result), nil
}

func encodePullRequestResult(pullRequest githubapi.PullRequest) (json.RawMessage, error) {
	if pullRequest.ID <= 0 || pullRequest.NodeID == "" || pullRequest.Number <= 0 || pullRequest.HTMLURL == "" {
		return nil, ErrInvalidMutationReconciliation
	}
	return json.Marshal(struct {
		PullRequestID int64  `json:"pull_request_id"`
		NodeID        string `json:"node_id"`
		Number        int    `json:"number"`
		HTMLURL       string `json:"html_url"`
		HeadSHA       string `json:"head_sha"`
	}{pullRequest.ID, pullRequest.NodeID, pullRequest.Number, pullRequest.HTMLURL, pullRequest.Head.SHA})
}

func encodeReviewResult(review githubapi.Review) (json.RawMessage, error) {
	if review.ID <= 0 || review.NodeID == "" || review.User.ID <= 0 || review.HTMLURL == "" {
		return nil, ErrInvalidMutationReconciliation
	}
	return json.Marshal(struct {
		ReviewID int64  `json:"review_id"`
		NodeID   string `json:"node_id"`
		State    string `json:"state"`
		CommitID string `json:"commit_id"`
		ActorID  int64  `json:"actor_id"`
		HTMLURL  string `json:"html_url"`
	}{review.ID, review.NodeID, review.State, review.CommitID, review.User.ID, review.HTMLURL})
}

func (reconciler *ProductionReconciler) developerCredential(ctx context.Context, repository store.AgentTurnRepository) (string, error) {
	credential, err := reconciler.credentials.DeveloperCredential(ctx, RepositoryScope{ID: repository.ID, Owner: repository.Owner, Name: repository.Name})
	if err != nil || !safeCredential(credential) {
		return "", dependencyError("resolve Developer credential")
	}
	return credential, nil
}

func (reconciler *ProductionReconciler) reviewerCredential(ctx context.Context, repository store.AgentTurnRepository) (string, error) {
	credential, err := reconciler.credentials.ReviewerCredential(ctx, RepositoryScope{ID: repository.ID, Owner: repository.Owner, Name: repository.Name})
	if err != nil || !safeCredential(credential) {
		return "", dependencyError("resolve Reviewer credential")
	}
	return credential, nil
}

func validReconciliationScope(reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) bool {
	valid := reconciliation.WorkflowID != "" && (reconciliation.Role == workflow.RoleDeveloper || reconciliation.Role == workflow.RoleReviewer) &&
		reconciliation.Repository.ID > 0 && validRepositoryPart(reconciliation.Repository.Owner) && validRepositoryPart(reconciliation.Repository.Name) &&
		reconciliation.Issue.ID > 0 && reconciliation.Issue.Number > 0 && reconciliation.Turn.ID != "" && reconciliation.Turn.AgentAssignmentID != "" && reconciliation.Turn.ExecutionEpoch > 0 &&
		validArtifactOperationID(mutation.ID) && mutation.AgentTurnID == reconciliation.Turn.ID && mutation.ExecutionEpoch == reconciliation.Turn.ExecutionEpoch && mutation.InvocationNumber > 0 &&
		(mutation.State == store.MutationUnknown || mutation.State == store.MutationReconciling) && mutation.OperationID != "" && len(mutation.Request) != 0
	if !valid {
		return false
	}
	if reconciliation.ChangeProposal == nil {
		return reconciliation.Turn.ChangeProposalID == "" && reconciliation.Turn.ExpectedHeadSHA == ""
	}
	return reconciliation.Turn.ChangeProposalID == reconciliation.ChangeProposal.ID && validRevision(reconciliation.Turn.ExpectedHeadSHA)
}

func exactTurnExpectedHead(reconciliation store.AgentTurnMutationReconciliationContext, mutation store.MutationReservation) bool {
	return validRevision(mutation.ExpectedSHA) &&
		(reconciliation.Turn.ExpectedHeadSHA == "" || reconciliation.Turn.ExpectedHeadSHA == mutation.ExpectedSHA)
}

func decodePersistedRequest(raw json.RawMessage, target any) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	return decoder.Decode(target) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func exactNumericResource(resource string, repositoryID, targetID int64) bool {
	prefix, suffix, ok := strings.Cut(resource, ":")
	if !ok || strings.Contains(suffix, ":") {
		return false
	}
	return prefix == strconv.FormatInt(repositoryID, 10) && suffix == strconv.FormatInt(targetID, 10)
}

func exactBranchResource(resource string, repositoryID int64) (string, bool) {
	prefix, branch, ok := strings.Cut(resource, ":")
	if !ok || prefix != strconv.FormatInt(repositoryID, 10) || !validBranchResourceName(branch) {
		return "", false
	}
	return branch, true
}

func exactOpenPullRequestResource(resource string, repositoryID int64) (string, string, bool) {
	prefix, refs, ok := strings.Cut(resource, ":")
	if !ok || prefix != strconv.FormatInt(repositoryID, 10) {
		return "", "", false
	}
	head, base, ok := strings.Cut(refs, ":")
	if !ok || !validBranchResourceName(head) || !validBranchResourceName(base) {
		return "", "", false
	}
	return head, base, true
}

func validBranchResourceName(branch string) bool {
	if strings.TrimSpace(branch) != branch || branch == "" || strings.ContainsAny(branch, "~^:?*[\\\x00") {
		return false
	}
	for _, character := range branch {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func hasExactOperationMarker(body, workflowID, assignmentID, operationID string) bool {
	wanted := githubapi.Marker{WorkflowID: workflowID, AgentAssignmentID: assignmentID, OperationID: operationID}
	count := 0
	for _, marker := range githubapi.ParseMarkers(body) {
		if marker == wanted {
			count++
		}
	}
	return count == 1
}

func hasAssignmentMarker(body, workflowID, assignmentID string) bool {
	count := 0
	for _, marker := range githubapi.ParseMarkers(body) {
		if marker.WorkflowID == workflowID && marker.AgentAssignmentID == assignmentID && validArtifactOperationID(marker.OperationID) {
			count++
		}
	}
	return count == 1
}

func validArtifactOperationID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return true
}

func joinVisibleParts(parts ...string) string {
	visible := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			visible = append(visible, part)
		}
	}
	return strings.Join(visible, "\n\n")
}

func safeCredential(credential string) bool {
	if credential == "" {
		return false
	}
	for _, character := range credential {
		if character > unicode.MaxASCII || unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func foundReconciliation(result json.RawMessage) MutationReconciliationResult {
	return MutationReconciliationResult{Disposition: ReconciliationFound, Outcome: store.RecoveredMutationOutcome{State: store.MutationSucceeded, Result: result}}
}

func failedReconciliation(diagnostic string) MutationReconciliationResult {
	return MutationReconciliationResult{Disposition: ReconciliationDefinitelyFailed, Outcome: store.RecoveredMutationOutcome{State: store.MutationFailed, LastError: diagnostic}}
}

func unresolvedReconciliation() MutationReconciliationResult {
	return MutationReconciliationResult{Disposition: ReconciliationUnresolved}
}

func dependencyError(operation string) error {
	return fmt.Errorf("%w: %s", ErrMutationReconciliationDependency, operation)
}

func interfaceNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
