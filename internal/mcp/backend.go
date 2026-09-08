package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/gitremote"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var (
	ErrInvalidBackendConfiguration = errors.New("invalid production tool backend configuration")
	ErrInvalidInvocation           = errors.New("invalid tool invocation")
	ErrToolNotAuthorized           = errors.New("tool is not authorized")
	ErrToolPrecondition            = errors.New("tool precondition failed")
	ErrToolDependency              = errors.New("tool dependency failed")
)

// GitHubAPI contains only the GitHub operations exposed as first-iteration tools.
type GitHubAPI interface {
	GetIssue(context.Context, string, string, string, int) (githubapi.Issue, error)
	ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error)
	GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error)
	ListPullRequests(context.Context, string, string, string, githubapi.ListPullRequestsRequest) ([]githubapi.PullRequest, error)
	ListPullRequestFiles(context.Context, string, string, string, int) ([]githubapi.PullRequestFile, error)
	ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error)
	ListReviewThreads(context.Context, string, string, string, int) ([]githubapi.ReviewThread, error)
	GetCheckRuns(context.Context, string, string, string, string) ([]githubapi.CheckRun, error)
	OpenPullRequest(context.Context, string, string, string, githubapi.OpenPullRequestRequest) (githubapi.PullRequest, error)
	CreateIssueComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error)
	CreatePullRequestComment(context.Context, string, string, string, int, githubapi.CommentRequest) (githubapi.IssueComment, error)
	SubmitReview(context.Context, string, string, string, int, githubapi.ReviewRequest) (githubapi.Review, error)
}

// RepositoryCredentials resolves short-lived credentials under the identity for the active Role.
type RepositoryCredentials interface {
	DeveloperCredential(context.Context, RepositoryScope) (string, error)
	ReviewerCredential(context.Context, RepositoryScope) (string, error)
}

// WorkspacePublisher is implemented by workspace.Lifecycle.
type WorkspacePublisher interface {
	Publish(context.Context, workspace.Publication) (workspace.PublicationResult, error)
}

type RequestReviewMutation struct {
	Scope       ToolScope `json:"scope"`
	OperationID string    `json:"operation_id"`
	HeadSHA     string    `json:"head_sha"`
	Summary     string    `json:"summary"`
}

type ReportBlockedMutation struct {
	Scope       ToolScope `json:"scope"`
	OperationID string    `json:"operation_id"`
	Reason      string    `json:"reason"`
	Details     string    `json:"details,omitempty"`
}

// WorkflowMutations performs orchestrator-owned mutations and returns credential-free durable JSON objects.
type WorkflowMutations interface {
	RequestReview(context.Context, RequestReviewMutation) (json.RawMessage, error)
	ReportBlocked(context.Context, ReportBlockedMutation) (json.RawMessage, error)
}

// LedgerWorkflowMutations returns workflow intents that become durable with the gateway's mutation completion.
// Phase 8 consumes these terminal mutation results when it settles the Agent Turn.
type LedgerWorkflowMutations struct{}

func (LedgerWorkflowMutations) RequestReview(_ context.Context, request RequestReviewMutation) (json.RawMessage, error) {
	if request.Scope.PullRequest == nil || request.OperationID == "" || request.HeadSHA == "" || strings.TrimSpace(request.Summary) == "" {
		return nil, ErrInvalidInvocation
	}
	return json.Marshal(struct {
		Outcome           string `json:"outcome"`
		PullRequestID     int64  `json:"pull_request_id"`
		PullRequestNumber int64  `json:"pull_request_number"`
		HeadSHA           string `json:"head_sha"`
	}{"REVIEW_REQUESTED", request.Scope.PullRequest.ID, request.Scope.PullRequest.Number, request.HeadSHA})
}

func (LedgerWorkflowMutations) ReportBlocked(_ context.Context, request ReportBlockedMutation) (json.RawMessage, error) {
	if request.OperationID == "" || strings.TrimSpace(request.Reason) == "" {
		return nil, ErrInvalidInvocation
	}
	return json.Marshal(struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Details string `json:"details,omitempty"`
	}{"BLOCKED", request.Reason, request.Details})
}

var _ WorkflowMutations = LedgerWorkflowMutations{}

type ProductionBackendConfig struct {
	GitHub           GitHubAPI
	Credentials      RepositoryCredentials
	Publisher        WorkspacePublisher
	Workflow         WorkflowMutations
	GitRemoteBaseURL string
}

type ProductionBackend struct {
	github      GitHubAPI
	credentials RepositoryCredentials
	publisher   WorkspacePublisher
	workflow    WorkflowMutations
	remoteBase  gitremote.BaseURL
	identity    workspace.CommitIdentity

	publicationMutex  sync.Mutex
	publicationLocks  map[publicationTurn]*sync.Mutex
	publishedHeads    map[publicationTurn]publicationHead
	plannedHeads      map[publicationPlan]string
	restoredMutations map[publicationTurn]map[string][sha256.Size]byte
}

type publicationTurn struct {
	assignmentID string
	turnID       string
	epoch        int64
}

type publicationPlan struct {
	turn        publicationTurn
	toolName    string
	operationID string
}

type publicationHead struct {
	head         string
	branchExists bool
	pullRequest  *PullRequestScope
}

var (
	_ Backend                = (*ProductionBackend)(nil)
	_ MutationPlanner        = (*ProductionBackend)(nil)
	_ MutationReplayRestorer = (*ProductionBackend)(nil)
	_ TurnReleaser           = (*ProductionBackend)(nil)
	_ GitHubAPI              = (*githubapi.APIClient)(nil)
	_ WorkspacePublisher     = (*workspace.Lifecycle)(nil)
)

func NewProductionBackend(config ProductionBackendConfig) (*ProductionBackend, error) {
	if config.GitHub == nil || config.Credentials == nil || config.Publisher == nil || config.Workflow == nil {
		return nil, ErrInvalidBackendConfiguration
	}
	remoteBase, err := gitremote.ParseBaseURL(config.GitRemoteBaseURL)
	if err != nil {
		return nil, ErrInvalidBackendConfiguration
	}
	return &ProductionBackend{
		github: config.GitHub, credentials: config.Credentials, publisher: config.Publisher,
		workflow: config.Workflow, remoteBase: remoteBase,
		identity:         workspace.CommitIdentity{Name: "Omnigrex Developer", Email: "developer@omnigrex.invalid"},
		publicationLocks: make(map[publicationTurn]*sync.Mutex), publishedHeads: make(map[publicationTurn]publicationHead),
		plannedHeads:      make(map[publicationPlan]string),
		restoredMutations: make(map[publicationTurn]map[string][sha256.Size]byte),
	}, nil
}

func (backend *ProductionBackend) Execute(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	tool, err := validateBackendInvocation(invocation)
	if err != nil {
		return nil, err
	}
	switch tool.Name {
	case ToolRequestReview:
		return backend.requestReview(ctx, invocation)
	case ToolReportBlocked:
		return backend.reportBlocked(ctx, invocation)
	}
	credential, err := backend.credential(ctx, tool.Name, invocation.Scope.Role, invocation.Scope.Repository)
	if err != nil {
		return nil, ErrToolDependency
	}
	owner := invocation.Scope.Repository.Owner
	repository := invocation.Scope.Repository.Name
	issueNumber := int(invocation.Scope.Issue.Number)
	pullRequestNumber := 0
	if invocation.Scope.PullRequest != nil {
		pullRequestNumber = int(invocation.Scope.PullRequest.Number)
	}

	var result any
	switch tool.Name {
	case ToolGetIssue:
		issue, callErr := backend.github.GetIssue(ctx, credential, owner, repository, issueNumber)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		if issue.ID != invocation.Scope.Issue.ID || int64(issue.Number) != invocation.Scope.Issue.Number {
			return nil, ErrToolPrecondition
		}
		result = issue
	case ToolListIssueComments:
		comments, callErr := backend.github.ListIssueComments(ctx, credential, owner, repository, issueNumber)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		result = comments
	case ToolGetPullRequest:
		pullRequest, callErr := backend.github.GetPullRequest(ctx, credential, owner, repository, pullRequestNumber)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		if !matchesScopedPullRequest(pullRequest, invocation.Scope) {
			return nil, ErrToolPrecondition
		}
		result = pullRequest
	case ToolListPullRequestReviews:
		reviews, callErr := backend.github.ListPullRequestReviews(ctx, credential, owner, repository, pullRequestNumber)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		result = reviews
	case ToolListReviewThreads:
		threads, callErr := backend.github.ListReviewThreads(ctx, credential, owner, repository, pullRequestNumber)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		result = threads
	case ToolGetCheckRuns:
		checks, callErr := backend.github.GetCheckRuns(ctx, credential, owner, repository, invocation.Scope.HeadSHA)
		if callErr != nil {
			return nil, ErrToolDependency
		}
		for _, check := range checks {
			if check.HeadSHA != invocation.Scope.HeadSHA {
				return nil, ErrToolPrecondition
			}
		}
		result = checks
	case ToolPublishChanges:
		return backend.publishChanges(ctx, invocation, credential)
	case ToolOpenPR:
		return backend.openPullRequest(ctx, invocation, credential)
	case ToolCommentOnIssue:
		return backend.commentOnIssue(ctx, invocation, credential)
	case ToolCommentOnPullRequest:
		return backend.commentOnPullRequest(ctx, invocation, credential)
	case ToolSubmitReview:
		return backend.submitReview(ctx, invocation, credential)
	default:
		return nil, ErrInvalidInvocation
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, ErrToolDependency
	}
	return encoded, nil
}

func (backend *ProductionBackend) PlanMutation(_ context.Context, invocation Invocation) (MutationMetadata, error) {
	tool, err := validateBackendInvocation(invocation)
	if err != nil {
		return MutationMetadata{}, err
	}
	if tool.Class != MutationTool {
		return MutationMetadata{}, ErrInvalidInvocation
	}
	metadata := invocation.Mutation
	if tool.Name == ToolPublishChanges || tool.Name == ToolOpenPR || tool.Name == ToolRequestReview {
		turnKey := publicationTurnKey(invocation.Scope)
		planKey := publicationPlan{turn: turnKey, toolName: invocation.Name, operationID: invocation.OperationID}
		backend.publicationMutex.Lock()
		head, exists := backend.plannedHeads[planKey]
		if !exists {
			head = invocation.Scope.HeadSHA
			if progress, published := backend.publishedHeads[turnKey]; published {
				head = progress.head
			}
			backend.plannedHeads[planKey] = head
		}
		backend.publicationMutex.Unlock()
		metadata.ExpectedSHA = head
	}
	return metadata, nil
}

// RestoreMutationReplay validates durable ancestor evidence and restores only exact-turn derived publication state.
func (backend *ProductionBackend) RestoreMutationReplay(_ context.Context, invocation Invocation, source store.MutationReservation) error {
	tool, err := validateMutationReplay(invocation, source)
	if err != nil {
		return err
	}
	if tool.Name != ToolPublishChanges && tool.Name != ToolOpenPR && tool.Name != ToolRequestReview {
		return nil
	}

	turnKey := publicationTurnKey(invocation.Scope)
	lock := backend.publicationLock(turnKey)
	lock.Lock()
	defer lock.Unlock()

	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	fingerprint := mutationReplayFingerprint(source)
	if restored := backend.restoredMutations[turnKey]; restored != nil {
		if previous, ok := restored[source.ID]; ok {
			if previous != fingerprint {
				return ErrToolPrecondition
			}
			return nil
		}
	}

	progress, exists := backend.publishedHeads[turnKey]
	if !exists {
		progress = publicationHead{head: invocation.Scope.HeadSHA, branchExists: invocation.Scope.PullRequest != nil}
	}
	if invocation.Scope.PullRequest != nil {
		if progress.pullRequest != nil && *progress.pullRequest != *invocation.Scope.PullRequest {
			return ErrToolPrecondition
		}
		if progress.pullRequest == nil {
			pullRequest := *invocation.Scope.PullRequest
			progress.pullRequest = &pullRequest
		}
		progress.branchExists = true
	}
	if exists && progress.head != source.ExpectedSHA {
		return ErrToolPrecondition
	}

	switch tool.Name {
	case ToolPublishChanges:
		var result struct {
			Head    string `json:"head"`
			Branch  string `json:"branch"`
			Changed *bool  `json:"changed"`
		}
		if !decodeExactResult(source.Result, &result) || result.Changed == nil || !validRevision(result.Head) || result.Branch != invocation.Scope.Branch ||
			*result.Changed && result.Head == source.ExpectedSHA || !*result.Changed && result.Head != source.ExpectedSHA {
			return ErrToolPrecondition
		}
		progress.head = result.Head
		progress.branchExists = progress.branchExists || *result.Changed || result.Head != invocation.Scope.HeadSHA
	case ToolOpenPR:
		pullRequest, head, ok := restoredPullRequest(source.Result, source.ExpectedSHA)
		if !ok {
			return ErrToolPrecondition
		}
		if !sameOrMissingPullRequest(progress.pullRequest, pullRequest) {
			return ErrToolPrecondition
		}
		progress.head = head
		progress.branchExists = true
		if progress.pullRequest == nil {
			progress.pullRequest = pullRequest
		}
	case ToolRequestReview:
		var result struct {
			Outcome           string `json:"outcome"`
			PullRequestID     int64  `json:"pull_request_id"`
			PullRequestNumber int64  `json:"pull_request_number"`
			HeadSHA           string `json:"head_sha"`
		}
		if !decodeExactResult(source.Result, &result) {
			return ErrToolPrecondition
		}
		pullRequest := &PullRequestScope{ID: result.PullRequestID, Number: result.PullRequestNumber}
		if result.Outcome != "REVIEW_REQUESTED" || pullRequest.ID <= 0 || pullRequest.Number <= 0 ||
			result.HeadSHA != source.ExpectedSHA || !sameOrMissingPullRequest(progress.pullRequest, pullRequest) {
			return ErrToolPrecondition
		}
		progress.head = result.HeadSHA
		progress.branchExists = true
		if progress.pullRequest == nil {
			progress.pullRequest = pullRequest
		}
	}

	backend.publishedHeads[turnKey] = progress
	if backend.restoredMutations[turnKey] == nil {
		backend.restoredMutations[turnKey] = make(map[string][sha256.Size]byte)
	}
	backend.restoredMutations[turnKey][source.ID] = fingerprint
	backend.plannedHeads[publicationPlan{turn: turnKey, toolName: source.ToolName, operationID: source.OperationID}] = source.ExpectedSHA
	return nil
}

// ReleaseTurn retires mutation planning and publication state after the gateway drains the registration.
func (backend *ProductionBackend) ReleaseTurn(scope ToolScope) {
	turnKey := publicationTurnKey(scope)
	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	delete(backend.publicationLocks, turnKey)
	delete(backend.publishedHeads, turnKey)
	delete(backend.restoredMutations, turnKey)
	for planKey := range backend.plannedHeads {
		if planKey.turn == turnKey {
			delete(backend.plannedHeads, planKey)
		}
	}
}

func (backend *ProductionBackend) requestReview(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil {
		return nil, ErrInvalidInvocation
	}
	effectiveScope := cloneToolScope(invocation.Scope)
	if effectiveScope.PullRequest == nil {
		effectiveScope.PullRequest = backend.currentPullRequest(invocation.Scope)
	}
	if effectiveScope.PullRequest == nil {
		return nil, ErrToolPrecondition
	}
	result, err := backend.workflow.RequestReview(ctx, RequestReviewMutation{
		Scope: effectiveScope, OperationID: invocation.OperationID,
		HeadSHA: backend.currentPublishedHead(invocation.Scope), Summary: arguments.Summary,
	})
	return canonicalWorkflowResult(result, err)
}

func (backend *ProductionBackend) reportBlocked(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments struct {
		Reason  string `json:"reason"`
		Details string `json:"details"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil {
		return nil, ErrInvalidInvocation
	}
	result, err := backend.workflow.ReportBlocked(ctx, ReportBlockedMutation{
		Scope: cloneToolScope(invocation.Scope), OperationID: invocation.OperationID,
		Reason: arguments.Reason, Details: arguments.Details,
	})
	return canonicalWorkflowResult(result, err)
}

func canonicalWorkflowResult(result json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		if IsOutcomeUnknown(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, OutcomeUnknown(ErrToolDependency)
		}
		return nil, ErrToolDependency
	}
	canonical, canonicalErr := canonicalJSON(result)
	if canonicalErr != nil {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(canonical, &object) != nil || len(object) == 0 {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	return canonical, nil
}

func cloneToolScope(scope ToolScope) ToolScope {
	if scope.PullRequest != nil {
		pullRequest := *scope.PullRequest
		scope.PullRequest = &pullRequest
	}
	return scope
}

func (backend *ProductionBackend) openPullRequest(ctx context.Context, invocation Invocation, credential string) (json.RawMessage, error) {
	if backend.currentPullRequest(invocation.Scope) != nil {
		return nil, ErrToolPrecondition
	}
	var arguments struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil {
		return nil, ErrInvalidInvocation
	}
	if !markerFree(arguments.Body) {
		return nil, ErrInvalidInvocation
	}
	marker, err := operationMarker(invocation)
	if err != nil {
		return nil, err
	}
	expectedHead := backend.currentPublishedHead(invocation.Scope)
	pullRequest, err := backend.github.OpenPullRequest(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name, githubapi.OpenPullRequestRequest{
		Title: arguments.Title, Body: arguments.Body, Head: invocation.Scope.Branch, Base: invocation.Scope.DefaultBranch,
		IssueNumber: int(invocation.Scope.Issue.Number), Marker: marker,
	})
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	if pullRequest.ID <= 0 || pullRequest.NodeID == "" || pullRequest.Number <= 0 || pullRequest.Head.Ref != invocation.Scope.Branch ||
		pullRequest.Head.SHA != expectedHead || pullRequest.Base.Ref != invocation.Scope.DefaultBranch || pullRequest.HTMLURL == "" {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	backend.recordOpenedPullRequest(invocation.Scope, pullRequest)
	return json.Marshal(struct {
		PullRequestID int64  `json:"pull_request_id"`
		NodeID        string `json:"node_id"`
		Number        int    `json:"number"`
		HTMLURL       string `json:"html_url"`
		HeadSHA       string `json:"head_sha"`
	}{pullRequest.ID, pullRequest.NodeID, pullRequest.Number, pullRequest.HTMLURL, pullRequest.Head.SHA})
}

func (backend *ProductionBackend) commentOnIssue(ctx context.Context, invocation Invocation, credential string) (json.RawMessage, error) {
	var arguments struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil {
		return nil, ErrInvalidInvocation
	}
	if !markerFree(arguments.Body) {
		return nil, ErrInvalidInvocation
	}
	marker, err := operationMarker(invocation)
	if err != nil {
		return nil, err
	}
	comment, err := backend.github.CreateIssueComment(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name,
		int(invocation.Scope.Issue.Number), githubapi.CommentRequest{Body: arguments.Body, Marker: marker})
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	return encodeCommentResult(comment)
}

func (backend *ProductionBackend) commentOnPullRequest(ctx context.Context, invocation Invocation, credential string) (json.RawMessage, error) {
	var arguments struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil || invocation.Scope.PullRequest == nil {
		return nil, ErrInvalidInvocation
	}
	if !markerFree(arguments.Body) {
		return nil, ErrInvalidInvocation
	}
	marker, err := operationMarker(invocation)
	if err != nil {
		return nil, err
	}
	comment, err := backend.github.CreatePullRequestComment(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name,
		int(invocation.Scope.PullRequest.Number), githubapi.CommentRequest{Body: arguments.Body, Marker: marker})
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	return encodeCommentResult(comment)
}

func (backend *ProductionBackend) submitReview(ctx context.Context, invocation Invocation, credential string) (json.RawMessage, error) {
	if invocation.Scope.Role != workflow.RoleReviewer || invocation.Scope.PullRequest == nil {
		return nil, ErrToolNotAuthorized
	}
	var arguments struct {
		Event    githubapi.ReviewEvent    `json:"event"`
		Body     string                   `json:"body"`
		Comments []reviewCommentArguments `json:"comments"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil ||
		(arguments.Event != githubapi.ReviewApprove && arguments.Event != githubapi.ReviewRequestChanges) {
		return nil, ErrInvalidInvocation
	}
	if arguments.Event == githubapi.ReviewRequestChanges && strings.TrimSpace(arguments.Body) == "" && len(arguments.Comments) == 0 {
		return nil, ErrInvalidInvocation
	}
	if !markerFree(arguments.Body) {
		return nil, ErrInvalidInvocation
	}
	current, err := backend.github.GetPullRequest(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name,
		int(invocation.Scope.PullRequest.Number))
	if err != nil {
		return nil, ErrToolDependency
	}
	if !matchesScopedPullRequest(current, invocation.Scope) {
		return nil, ErrToolPrecondition
	}
	files, err := backend.github.ListPullRequestFiles(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name,
		int(invocation.Scope.PullRequest.Number))
	if err != nil {
		return nil, ErrToolDependency
	}
	if !reviewCommentsMatchFiles(arguments.Comments, files) {
		return nil, ErrToolPrecondition
	}
	reviewBody, err := githubapi.EnsureMarker(arguments.Body, githubapi.Marker{
		WorkflowID: invocation.Scope.WorkflowID, AgentAssignmentID: invocation.Scope.AgentAssignmentID, OperationID: invocation.OperationID,
	})
	if err != nil {
		return nil, ErrInvalidInvocation
	}
	comments := make([]githubapi.ReviewCommentRequest, len(arguments.Comments))
	for index, comment := range arguments.Comments {
		comments[index] = githubapi.ReviewCommentRequest{
			Path: comment.Path, Body: comment.Body, Line: comment.Line, Side: comment.Side,
			StartLine: comment.StartLine, StartSide: comment.StartSide,
		}
	}
	review, err := backend.github.SubmitReview(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name,
		int(invocation.Scope.PullRequest.Number), githubapi.ReviewRequest{
			Body: reviewBody, CommitID: invocation.Scope.HeadSHA, Event: arguments.Event, Comments: comments,
		})
	if err != nil {
		return nil, classifyGitHubMutationError(err)
	}
	expectedState := "APPROVED"
	if arguments.Event == githubapi.ReviewRequestChanges {
		expectedState = "CHANGES_REQUESTED"
	}
	if review.ID <= 0 || review.NodeID == "" || review.User.ID <= 0 || review.CommitID != invocation.Scope.HeadSHA || review.State != expectedState || review.HTMLURL == "" {
		return nil, OutcomeUnknown(ErrToolPrecondition)
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

type reviewDiffHunk struct {
	leftStart, leftCount   int
	rightStart, rightCount int
	leftLines, rightLines  map[int]struct{}
}

type reviewCommentArguments struct {
	Path      string               `json:"path"`
	Body      string               `json:"body"`
	Line      int                  `json:"line"`
	Side      githubapi.ReviewSide `json:"side"`
	StartLine int                  `json:"start_line"`
	StartSide githubapi.ReviewSide `json:"start_side"`
}

func reviewCommentsMatchFiles(comments []reviewCommentArguments, files []githubapi.PullRequestFile) bool {
	patches := make(map[string]*string, len(files))
	duplicates := make(map[string]bool)
	for _, file := range files {
		if _, exists := patches[file.Filename]; exists {
			duplicates[file.Filename] = true
		}
		patches[file.Filename] = file.Patch
	}
	parsed := make(map[string][]reviewDiffHunk)
	for _, comment := range comments {
		patch, exists := patches[comment.Path]
		if !exists || duplicates[comment.Path] || patch == nil {
			return false
		}
		hunks, parsedAlready := parsed[comment.Path]
		if !parsedAlready {
			var valid bool
			hunks, valid = parseReviewDiffHunks(*patch)
			if !valid {
				return false
			}
			parsed[comment.Path] = hunks
		}
		startLine := comment.Line
		if comment.StartLine == 0 {
			if comment.StartSide != "" {
				return false
			}
		} else {
			if comment.StartSide != comment.Side || comment.StartLine >= comment.Line {
				return false
			}
			startLine = comment.StartLine
		}
		if !reviewRangeInHunk(hunks, comment.Side, startLine, comment.Line) {
			return false
		}
	}
	return true
}

func reviewRangeInHunk(hunks []reviewDiffHunk, side githubapi.ReviewSide, start, end int) bool {
	if start <= 0 || end < start {
		return false
	}
	for _, hunk := range hunks {
		lines := hunk.rightLines
		if side == githubapi.ReviewSideLeft {
			lines = hunk.leftLines
		} else if side != githubapi.ReviewSideRight {
			return false
		}
		valid := true
		for line := start; line <= end; line++ {
			if _, exists := lines[line]; !exists {
				valid = false
				break
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func parseReviewDiffHunks(patch string) ([]reviewDiffHunk, bool) {
	var hunks []reviewDiffHunk
	var current reviewDiffHunk
	leftLines, rightLines := 0, 0
	inHunk, changed, previousBody := false, false, false
	finishHunk := func() bool {
		if !inHunk || leftLines != current.leftCount || rightLines != current.rightCount || !changed {
			return false
		}
		hunks = append(hunks, current)
		return true
	}
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			if inHunk && !finishHunk() {
				return nil, false
			}
			var valid bool
			current, valid = parseReviewDiffHunkHeader(line)
			if !valid {
				return nil, false
			}
			current.leftLines = make(map[int]struct{})
			current.rightLines = make(map[int]struct{})
			leftLines, rightLines = 0, 0
			inHunk, changed, previousBody = true, false, false
			continue
		}
		if !inHunk {
			return nil, false
		}
		if line == `\ No newline at end of file` {
			if !previousBody {
				return nil, false
			}
			previousBody = false
			continue
		}
		if line == "" {
			return nil, false
		}
		switch line[0] {
		case ' ':
			leftLines++
			rightLines++
			current.rightLines[current.rightStart+rightLines-1] = struct{}{}
		case '-':
			leftLines++
			current.leftLines[current.leftStart+leftLines-1] = struct{}{}
			changed = true
		case '+':
			rightLines++
			current.rightLines[current.rightStart+rightLines-1] = struct{}{}
			changed = true
		default:
			return nil, false
		}
		if leftLines > current.leftCount || rightLines > current.rightCount {
			return nil, false
		}
		previousBody = true
	}
	if !finishHunk() {
		return nil, false
	}
	return hunks, true
}

func parseReviewDiffHunkHeader(header string) (reviewDiffHunk, bool) {
	if !strings.HasPrefix(header, "@@ -") {
		return reviewDiffHunk{}, false
	}
	left, remainder, found := strings.Cut(strings.TrimPrefix(header, "@@ -"), " +")
	if !found {
		return reviewDiffHunk{}, false
	}
	right, suffix, found := strings.Cut(remainder, " @@")
	if !found || suffix != "" && !strings.HasPrefix(suffix, " ") {
		return reviewDiffHunk{}, false
	}
	leftStart, leftCount, leftValid := parseReviewDiffRange(left)
	rightStart, rightCount, rightValid := parseReviewDiffRange(right)
	return reviewDiffHunk{leftStart: leftStart, leftCount: leftCount, rightStart: rightStart, rightCount: rightCount}, leftValid && rightValid
}

func parseReviewDiffRange(value string) (int, int, bool) {
	startValue, countValue, hasCount := strings.Cut(value, ",")
	if !validReviewDiffDecimal(startValue) || hasCount && !validReviewDiffDecimal(countValue) {
		return 0, 0, false
	}
	start, err := strconv.Atoi(startValue)
	if err != nil {
		return 0, 0, false
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countValue)
		if err != nil {
			return 0, 0, false
		}
	}
	return start, count, start > 0 || start == 0 && count == 0
}

func validReviewDiffDecimal(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func operationMarker(invocation Invocation) (string, error) {
	marker, err := githubapi.RenderMarker(githubapi.Marker{
		WorkflowID: invocation.Scope.WorkflowID, AgentAssignmentID: invocation.Scope.AgentAssignmentID, OperationID: invocation.OperationID,
	})
	if err != nil {
		return "", ErrInvalidInvocation
	}
	return marker, nil
}

func markerFree(body string) bool {
	inspection := githubapi.InspectMarkers(body)
	return !inspection.Untrusted && len(inspection.Markers) == 0
}

func encodeCommentResult(comment githubapi.IssueComment) (json.RawMessage, error) {
	if comment.ID <= 0 || comment.NodeID == "" || comment.HTMLURL == "" {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	return json.Marshal(struct {
		CommentID int64  `json:"comment_id"`
		NodeID    string `json:"node_id"`
		HTMLURL   string `json:"html_url"`
	}{comment.ID, comment.NodeID, comment.HTMLURL})
}

func classifyGitHubMutationError(err error) error {
	if IsOutcomeUnknown(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		return OutcomeUnknown(ErrToolDependency)
	}
	var transient *githubapi.TransientError
	var transport net.Error
	if errors.As(err, &transient) || errors.As(err, &transport) {
		return OutcomeUnknown(ErrToolDependency)
	}
	return ErrToolDependency
}

func (backend *ProductionBackend) currentPublishedHead(scope ToolScope) string {
	turnKey := publicationTurnKey(scope)
	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	if progress, ok := backend.publishedHeads[turnKey]; ok {
		return progress.head
	}
	return scope.HeadSHA
}

func (backend *ProductionBackend) currentPullRequest(scope ToolScope) *PullRequestScope {
	turnKey := publicationTurnKey(scope)
	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	progress, ok := backend.publishedHeads[turnKey]
	if !ok || progress.pullRequest == nil {
		return nil
	}
	pullRequest := *progress.pullRequest
	return &pullRequest
}

func (backend *ProductionBackend) recordOpenedPullRequest(scope ToolScope, pullRequest githubapi.PullRequest) {
	turnKey := publicationTurnKey(scope)
	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	progress, ok := backend.publishedHeads[turnKey]
	if !ok {
		progress = publicationHead{head: scope.HeadSHA}
	}
	progress.head = pullRequest.Head.SHA
	progress.branchExists = true
	progress.pullRequest = &PullRequestScope{ID: pullRequest.ID, Number: int64(pullRequest.Number)}
	backend.publishedHeads[turnKey] = progress
}

func publicationTurnKey(scope ToolScope) publicationTurn {
	return publicationTurn{assignmentID: scope.AgentAssignmentID, turnID: scope.AgentTurnID, epoch: scope.ExecutionEpoch}
}

func (backend *ProductionBackend) publishChanges(ctx context.Context, invocation Invocation, credential string) (json.RawMessage, error) {
	var arguments struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(invocation.Arguments, &arguments) != nil {
		return nil, ErrInvalidInvocation
	}
	turnKey := publicationTurnKey(invocation.Scope)
	lock := backend.publicationLock(turnKey)
	lock.Lock()
	defer lock.Unlock()

	backend.publicationMutex.Lock()
	progress, exists := backend.publishedHeads[turnKey]
	backend.publicationMutex.Unlock()
	if !exists {
		progress = publicationHead{head: invocation.Scope.HeadSHA, branchExists: invocation.Scope.PullRequest != nil}
	}
	expectedOldHead := ""
	if progress.branchExists {
		expectedOldHead = progress.head
	}
	repositoryURL, err := backend.remoteBase.RepositoryURL(invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name)
	if err != nil {
		return nil, ErrInvalidInvocation
	}
	result, err := backend.publisher.Publish(ctx, workspace.Publication{
		AssignmentID:  invocation.Scope.AgentAssignmentID,
		RepositoryURL: repositoryURL,
		Credential:    credential, BaseRevision: progress.head, ExpectedOldHead: expectedOldHead,
		Branch: invocation.Scope.Branch, Message: arguments.Message + "\n\nOmnigrex-Operation-ID: " + invocation.OperationID,
		Identity: backend.identity, Time: invocation.Scope.TurnCreatedAt.UTC(),
	})
	if err != nil {
		if errors.Is(err, workspace.ErrPushRejected) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, OutcomeUnknown(ErrToolDependency)
		}
		return nil, ErrToolDependency
	}
	if !validRevision(result.Head) || result.Changed && result.Head == progress.head || !result.Changed && result.Head != progress.head {
		return nil, OutcomeUnknown(ErrToolPrecondition)
	}
	progress.head = result.Head
	progress.branchExists = progress.branchExists || result.Changed
	backend.publicationMutex.Lock()
	backend.publishedHeads[turnKey] = progress
	backend.publicationMutex.Unlock()
	return json.Marshal(struct {
		Head    string `json:"head"`
		Branch  string `json:"branch"`
		Changed bool   `json:"changed"`
	}{Head: result.Head, Branch: invocation.Scope.Branch, Changed: result.Changed})
}

func (backend *ProductionBackend) publicationLock(turnKey publicationTurn) *sync.Mutex {
	backend.publicationMutex.Lock()
	defer backend.publicationMutex.Unlock()
	lock := backend.publicationLocks[turnKey]
	if lock == nil {
		lock = &sync.Mutex{}
		backend.publicationLocks[turnKey] = lock
	}
	return lock
}

func validateMutationReplay(invocation Invocation, source store.MutationReservation) (ToolDefinition, error) {
	tool, err := validateBackendInvocation(invocation)
	if err != nil || tool.Class != MutationTool || source.State != store.MutationSucceeded ||
		source.ID == "" || invocation.OperationID != source.ID || source.AgentTurnID == "" ||
		source.AgentTurnID == invocation.Scope.AgentTurnID || source.ExecutionEpoch <= 0 ||
		source.ToolName != invocation.Name || !validOperationID(source.OperationID) {
		return ToolDefinition{}, ErrInvalidInvocation
	}
	operationID, err := requiredStringArgument(invocation.Arguments, "operation_id")
	if err != nil || operationID != source.OperationID {
		return ToolDefinition{}, ErrInvalidInvocation
	}
	request, err := canonicalJSON(source.Request)
	if err != nil || !bytes.Equal(request, source.Request) || !bytes.Equal(request, invocation.Arguments) {
		return ToolDefinition{}, ErrInvalidInvocation
	}
	result, err := canonicalJSON(source.Result)
	var object map[string]json.RawMessage
	if err != nil || json.Unmarshal(result, &object) != nil || len(object) == 0 {
		return ToolDefinition{}, ErrToolPrecondition
	}
	expected := replayMetadataForScope(invocation.Name, invocation.Scope)
	if invocation.Mutation.ExternalService != source.ExternalService || invocation.Mutation.ExternalResourceID != source.ExternalResourceID ||
		invocation.Mutation.ExpectedSHA != source.ExpectedSHA || source.ExternalService != expected.ExternalService ||
		source.ExternalResourceID != expected.ExternalResourceID {
		return ToolDefinition{}, ErrToolPrecondition
	}
	publicationMutation := invocation.Name == ToolPublishChanges || invocation.Name == ToolOpenPR || invocation.Name == ToolRequestReview
	if publicationMutation {
		if !validRevision(source.ExpectedSHA) {
			return ToolDefinition{}, ErrToolPrecondition
		}
	} else if source.ExpectedSHA != expected.ExpectedSHA {
		return ToolDefinition{}, ErrToolPrecondition
	}
	return tool, nil
}

func mutationReplayFingerprint(source store.MutationReservation) [sha256.Size]byte {
	result, _ := canonicalJSON(source.Result)
	encoded, _ := json.Marshal(struct {
		AgentTurnID        string          `json:"agent_turn_id"`
		ExecutionEpoch     int64           `json:"execution_epoch"`
		OperationID        string          `json:"operation_id"`
		ToolName           string          `json:"tool_name"`
		Request            json.RawMessage `json:"request"`
		ExternalService    string          `json:"external_service"`
		ExternalResourceID string          `json:"external_resource_id"`
		ExpectedSHA        string          `json:"expected_sha"`
		Result             json.RawMessage `json:"result"`
	}{source.AgentTurnID, source.ExecutionEpoch, source.OperationID, source.ToolName, source.Request,
		source.ExternalService, source.ExternalResourceID, source.ExpectedSHA, result})
	return sha256.Sum256(encoded)
}

func replayMetadataForScope(tool string, scope ToolScope) MutationMetadata {
	metadata := MutationMetadata{ExternalService: "github"}
	switch tool {
	case ToolPublishChanges:
		metadata.ExternalService = "git"
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s", scope.Repository.ID, scope.Branch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolOpenPR:
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s:%s", scope.Repository.ID, scope.Branch, scope.DefaultBranch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolRequestReview:
		metadata.ExternalService = "omnigrex"
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s", scope.Repository.ID, scope.Branch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolReportBlocked:
		metadata.ExternalService = "omnigrex"
		metadata.ExternalResourceID = scope.WorkflowID
	case ToolSubmitReview, ToolCommentOnPullRequest:
		if scope.PullRequest != nil {
			metadata.ExternalResourceID = fmt.Sprintf("%d:%d", scope.Repository.ID, scope.PullRequest.ID)
		}
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolCommentOnIssue:
		metadata.ExternalResourceID = fmt.Sprintf("%d:%d", scope.Repository.ID, scope.Issue.ID)
	}
	return metadata
}

func restoredPullRequest(raw json.RawMessage, expectedHead string) (*PullRequestScope, string, bool) {
	var result struct {
		PullRequestID int64  `json:"pull_request_id"`
		NodeID        string `json:"node_id"`
		Number        int64  `json:"number"`
		HTMLURL       string `json:"html_url"`
		HeadSHA       string `json:"head_sha"`
	}
	if !decodeExactResult(raw, &result) || result.PullRequestID <= 0 || result.NodeID == "" || result.Number <= 0 ||
		result.HTMLURL == "" || result.HeadSHA != expectedHead {
		return nil, "", false
	}
	return &PullRequestScope{ID: result.PullRequestID, Number: result.Number}, result.HeadSHA, true
}

func sameOrMissingPullRequest(current, restored *PullRequestScope) bool {
	return current == nil || restored != nil && *current == *restored
}

func decodeExactResult(raw json.RawMessage, destination any) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func validateBackendInvocation(invocation Invocation) (ToolDefinition, error) {
	tool, found := definition(invocation.Name)
	if !found || tool.Class != invocation.Class || !slices.Contains(roleTools[invocation.Scope.Role], invocation.Name) {
		return ToolDefinition{}, ErrToolNotAuthorized
	}
	if validateArguments(invocation.Arguments, tool.InputSchema) != nil || !validToolScope(invocation.Scope) {
		return ToolDefinition{}, ErrInvalidInvocation
	}
	if !scopeAllowsBackendTool(invocation.Scope, invocation.Name) {
		return ToolDefinition{}, ErrToolPrecondition
	}
	if tool.Class == MutationTool {
		if _, err := requiredStringArgument(invocation.Arguments, "operation_id"); err != nil || !validOperationID(invocation.OperationID) {
			return ToolDefinition{}, ErrInvalidInvocation
		}
	} else if invocation.OperationID != "" {
		return ToolDefinition{}, ErrInvalidInvocation
	}
	return tool, nil
}

func validToolScope(scope ToolScope) bool {
	valid := scope.WorkflowID != "" && scope.AgentAssignmentID != "" && scope.AgentSessionID != "" && scope.AgentTurnID != "" &&
		scope.ExecutionEpoch > 0 && (scope.Role == workflow.RoleDeveloper || scope.Role == workflow.RoleReviewer) &&
		scope.Repository.ID > 0 && validRepositoryPart(scope.Repository.Owner) && validRepositoryPart(scope.Repository.Name) &&
		scope.Issue.ID > 0 && scope.Issue.Number > 0 && scope.Issue.Number <= int64(^uint(0)>>1) &&
		validBranchResourceName(scope.Branch) && validBranchResourceName(scope.DefaultBranch) && validRevision(scope.HeadSHA) && !scope.TurnCreatedAt.IsZero()
	if !valid || scope.PullRequest != nil && (scope.PullRequest.ID <= 0 || scope.PullRequest.Number <= 0 || scope.PullRequest.Number > int64(^uint(0)>>1)) {
		return false
	}
	return scope.Role != workflow.RoleReviewer || scope.PullRequest != nil
}

func validRepositoryPart(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && !strings.ContainsAny(value, "/\\")
}

func validRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return true
}

func scopeAllowsBackendTool(scope ToolScope, tool string) bool {
	switch tool {
	case ToolGetPullRequest, ToolListPullRequestReviews, ToolListReviewThreads,
		ToolSubmitReview, ToolCommentOnPullRequest:
		return scope.PullRequest != nil && scope.PullRequest.ID > 0 && scope.PullRequest.Number > 0 && scope.PullRequest.Number <= int64(^uint(0)>>1)
	case ToolOpenPR:
		return scope.PullRequest == nil
	default:
		return true
	}
}

func matchesScopedPullRequest(pullRequest githubapi.PullRequest, scope ToolScope) bool {
	return scope.PullRequest != nil && pullRequest.ID == scope.PullRequest.ID && int64(pullRequest.Number) == scope.PullRequest.Number &&
		pullRequest.Head.SHA == scope.HeadSHA && pullRequest.Head.Ref == scope.Branch && pullRequest.Base.Ref == scope.DefaultBranch
}

func (backend *ProductionBackend) credential(ctx context.Context, tool string, role workflow.Role, repository RepositoryScope) (string, error) {
	var credential string
	var err error
	developerPermission := role == workflow.RoleDeveloper
	switch tool {
	case ToolGetIssue, ToolListIssueComments, ToolGetCheckRuns, ToolCommentOnIssue, ToolCommentOnPullRequest:
		developerPermission = true
	}
	if developerPermission {
		credential, err = backend.credentials.DeveloperCredential(ctx, repository)
	} else {
		credential, err = backend.credentials.ReviewerCredential(ctx, repository)
	}
	if err != nil || credential == "" {
		return "", ErrToolDependency
	}
	for _, character := range credential {
		if character > unicode.MaxASCII || unicode.IsSpace(character) || unicode.IsControl(character) {
			return "", ErrToolDependency
		}
	}
	return credential, nil
}

func (backend *ProductionBackend) String() string {
	return fmt.Sprintf("production MCP backend for %s", backend.remoteBase.String())
}

func (backend *ProductionBackend) GoString() string {
	return "mcp.ProductionBackend{dependencies:<redacted>}"
}
