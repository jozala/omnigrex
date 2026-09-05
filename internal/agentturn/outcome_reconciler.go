package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var (
	ErrInvalidOutcomeReconciler         = errors.New("invalid Agent Turn outcome reconciler")
	ErrInvalidOutcomeReconciliation     = errors.New("invalid Agent Turn outcome reconciliation")
	ErrOutcomeReconciliationUnavailable = errors.New("Agent Turn outcome reconciliation is unavailable")
)

const maxOutcomeDiagnosticRunes = 4096

// PromptErrorClassification is the credential-free terminal classification of an ACP prompt error.
type PromptErrorClassification string

const (
	PromptErrorDeadline     PromptErrorClassification = "DEADLINE"
	PromptErrorCancellation PromptErrorClassification = "CANCELLATION"
	PromptErrorFailure      PromptErrorClassification = "FAILURE"
)

// ClassifyPromptError maps transport errors to the terminal Agent Turn status contract.
func ClassifyPromptError(err error) PromptErrorClassification {
	if errors.Is(err, context.DeadlineExceeded) {
		return PromptErrorDeadline
	}
	if errors.Is(err, context.Canceled) {
		return PromptErrorCancellation
	}
	return PromptErrorFailure
}

// OutcomeReconcilerStore is the durable evidence surface needed after MCP mutation admission closes.
type OutcomeReconcilerStore interface {
	ListAgentTurnMutationInvocations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error)
	GetChangeProposalReview(context.Context, int64, int64) (*workflow.ReviewIdentity, error)
}

// OutcomeReconcilerGitHub is the fresh GitHub observation surface used to corroborate terminal evidence.
type OutcomeReconcilerGitHub interface {
	GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error)
	ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error)
}

type outcomeReconcilerClock interface {
	Now() time.Time
}

type realOutcomeReconcilerClock struct{}

func (realOutcomeReconcilerClock) Now() time.Time { return time.Now() }

// OutcomeReconcilerConfig supplies only read dependencies and a testable observation clock.
type OutcomeReconcilerConfig struct {
	Store  OutcomeReconcilerStore
	GitHub OutcomeReconcilerGitHub
	Clock  interface{ Now() time.Time }
}

// OutcomeReconciliation contains the exact acquired turn context and ephemeral Role credential.
// Exactly one of PromptResponse and PromptError must be supplied.
type OutcomeReconciliation struct {
	Lease                store.AgentTurnLease
	Execution            store.AgentTurnExecutionContext
	PromptResponse       *acp.PromptResponse
	PromptError          PromptErrorClassification
	PromptDiagnostic     string
	RepositoryCredential string
	Paths                workspace.Paths
}

func (OutcomeReconciliation) String() string { return "Agent Turn outcome reconciliation" }
func (OutcomeReconciliation) GoString() string {
	return "agentturn.OutcomeReconciliation{<credentials redacted>}"
}

// OutcomeReconciler derives settlement observations without consulting ACP output text.
type OutcomeReconciler struct {
	store  OutcomeReconcilerStore
	github OutcomeReconcilerGitHub
	clock  outcomeReconcilerClock
}

var (
	_ OutcomeReconcilerStore  = (*store.Store)(nil)
	_ OutcomeReconcilerGitHub = (*githubapi.APIClient)(nil)
)

func NewOutcomeReconciler(config OutcomeReconcilerConfig) (*OutcomeReconciler, error) {
	if nilInterface(config.Store) || nilInterface(config.GitHub) {
		return nil, ErrInvalidOutcomeReconciler
	}
	clock := outcomeReconcilerClock(realOutcomeReconcilerClock{})
	if config.Clock != nil {
		clock = config.Clock
	}
	if nilInterface(clock) {
		return nil, ErrInvalidOutcomeReconciler
	}
	return &OutcomeReconciler{store: config.Store, github: config.GitHub, clock: clock}, nil
}

// Reconcile reads the closed terminal ledger and corroborates its sole successful terminal intent.
func (reconciler *OutcomeReconciler) Reconcile(ctx context.Context, request OutcomeReconciliation) (store.AgentTurnSettlementObservation, error) {
	if reconciler == nil || !validOutcomeBinding(request.Lease, request.Execution) || !validPromptInput(request) {
		return store.AgentTurnSettlementObservation{}, ErrInvalidOutcomeReconciliation
	}
	mutations, err := reconciler.store.ListAgentTurnMutationInvocations(ctx, request.Lease)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAgentTurnMutationsUnsettled):
			return store.AgentTurnSettlementObservation{}, store.ErrAgentTurnMutationsUnsettled
		case errors.Is(err, store.ErrMutationAdmissionClosed):
			return store.AgentTurnSettlementObservation{}, store.ErrMutationAdmissionClosed
		case errors.Is(err, store.ErrAgentTurnFenceLost):
			return store.AgentTurnSettlementObservation{}, store.ErrAgentTurnFenceLost
		default:
			return store.AgentTurnSettlementObservation{}, ErrOutcomeReconciliationUnavailable
		}
	}
	observedAt := reconciler.clock.Now().UTC()
	if observedAt.IsZero() {
		return store.AgentTurnSettlementObservation{}, ErrInvalidOutcomeReconciler
	}
	promptOutcome := encodedPromptOutcome(request.PromptResponse)
	if ledgerHasUnsettledMutation(mutations) {
		return store.AgentTurnSettlementObservation{}, store.ErrAgentTurnMutationsUnsettled
	}
	if diagnostic := validateTerminalLedger(mutations, request.Lease); diagnostic != "" {
		return infrastructureObservation(observedAt, promptTerminalStatus(request), promptOutcome, diagnostic, request.RepositoryCredential), nil
	}
	if diagnostic := promptFailureDiagnostic(request); diagnostic != "" {
		return infrastructureObservation(observedAt, promptTerminalStatus(request), promptOutcome, diagnostic, request.RepositoryCredential), nil
	}

	intents := successfulTerminalIntents(mutations)
	if len(intents) == 0 {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "ACP prompt ended without a successful terminal mutation intent", request.RepositoryCredential), nil
	}
	if len(intents) != 1 {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "mutation ledger contains duplicate or conflicting terminal intents", request.RepositoryCredential), nil
	}
	intent := intents[0]
	if intent.ToolName == mcp.ToolReportBlocked {
		return blockedObservation(observedAt, promptOutcome, intent, request.Execution.WorkflowID, request.RepositoryCredential), nil
	}

	switch request.Execution.Assignment.Role {
	case workflow.RoleDeveloper:
		if intent.ToolName != mcp.ToolRequestReview {
			return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "Developer mutation ledger contains an invalid terminal intent", request.RepositoryCredential), nil
		}
		return reconciler.reconcileDeveloper(ctx, request, observedAt, promptOutcome, mutations, intent), nil
	case workflow.RoleReviewer:
		if intent.ToolName != mcp.ToolSubmitReview {
			return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "Reviewer mutation ledger contains an invalid terminal intent", request.RepositoryCredential), nil
		}
		return reconciler.reconcileReviewer(ctx, request, observedAt, promptOutcome, intent), nil
	default:
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "Agent Turn has an invalid Role", request.RepositoryCredential), nil
	}
}

func (reconciler *OutcomeReconciler) reconcileDeveloper(ctx context.Context, request OutcomeReconciliation, observedAt time.Time, promptOutcome json.RawMessage, mutations []store.MutationReservation, intent store.MutationReservation) store.AgentTurnSettlementObservation {
	failure := func(diagnostic string) store.AgentTurnSettlementObservation {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, diagnostic, request.RepositoryCredential)
	}
	if strings.TrimSpace(request.RepositoryCredential) == "" {
		return failure("Developer repository credential is unavailable")
	}
	var arguments struct {
		OperationID string `json:"operation_id"`
		Summary     string `json:"summary"`
	}
	var result struct {
		Outcome           string `json:"outcome"`
		PullRequestID     int64  `json:"pull_request_id"`
		PullRequestNumber int64  `json:"pull_request_number"`
		HeadSHA           string `json:"head_sha"`
	}
	branch, resourceOK := exactBranchResource(intent.ExternalResourceID, request.Execution.Repository.ID)
	if intent.ExternalService != "omnigrex" || !decodeExactObject(intent.Request, &arguments) ||
		arguments.OperationID != intent.OperationID || strings.TrimSpace(arguments.Summary) == "" ||
		!decodeExactObject(intent.Result, &result) || result.Outcome != "REVIEW_REQUESTED" ||
		result.PullRequestID <= 0 || result.PullRequestNumber <= 0 || strings.TrimSpace(result.HeadSHA) == "" ||
		intent.ExpectedSHA != result.HeadSHA || !resourceOK {
		return failure("request_review terminal evidence is malformed or incoherent")
	}

	baseRef := ""
	var opened *openPullRequestEvidence
	for _, mutation := range mutations {
		if mutation.State != store.MutationSucceeded || mutation.ToolName != mcp.ToolOpenPR {
			continue
		}
		if opened != nil || request.Execution.ChangeProposal != nil {
			return failure("mutation ledger contains conflicting open_pr evidence")
		}
		evidence, ok := parseOpenPullRequestEvidence(mutation, request.Execution.Repository.ID)
		if !ok {
			return failure("open_pr terminal evidence is malformed or incoherent")
		}
		opened = &evidence
		baseRef = evidence.BaseRef
	}
	if request.Execution.ChangeProposal != nil {
		proposal := request.Execution.ChangeProposal
		if proposal.PullRequestID != result.PullRequestID || proposal.PullRequestNumber != result.PullRequestNumber ||
			proposal.HeadRef != branch || proposal.BaseRef == "" {
			return failure("request_review evidence conflicts with the durable Change Proposal")
		}
		baseRef = proposal.BaseRef
	} else if opened != nil && (opened.PullRequestID != result.PullRequestID || opened.PullRequestNumber != result.PullRequestNumber || opened.HeadSHA != result.HeadSHA || opened.HeadRef != branch) {
		return failure("request_review evidence conflicts with open_pr evidence")
	}

	pullRequest, err := reconciler.github.GetPullRequest(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(result.PullRequestNumber))
	if err != nil {
		return failure("fresh Developer Pull Request observation failed")
	}
	if baseRef == "" {
		baseRef = pullRequest.Base.Ref
	}
	if !matchesDeveloperPullRequest(pullRequest, request.Execution.Repository, result, branch, baseRef) {
		return failure("fresh Developer Pull Request does not match terminal evidence")
	}
	if opened != nil && (opened.NodeID != pullRequest.NodeID || opened.PullRequestID != pullRequest.ID || opened.PullRequestNumber != int64(pullRequest.Number)) {
		return failure("fresh Developer Pull Request conflicts with open_pr identity")
	}
	proposal := settlementChangeProposal(request.Execution, pullRequest)
	if containsCredentialInProposal(proposal, request.RepositoryCredential) {
		return failure("fresh Developer Pull Request contains unsafe credential material")
	}
	workspaceTree, err := workspace.SnapshotTree(request.Paths.Workspace)
	if err != nil {
		return failure("Developer workspace tree observation failed")
	}
	publicationTree, err := workspace.SnapshotTree(request.Paths.Publication)
	if err != nil {
		return failure("Developer publication tree observation failed")
	}
	if !workspaceTree.Equal(publicationTree) {
		return failure("Developer workspace has unpublished normalized tree changes")
	}
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeChangeProposalReady, ChangeProposal: proposal,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: promptOutcome},
	}
}

func (reconciler *OutcomeReconciler) reconcileReviewer(ctx context.Context, request OutcomeReconciliation, observedAt time.Time, promptOutcome json.RawMessage, intent store.MutationReservation) store.AgentTurnSettlementObservation {
	failure := func(diagnostic string) store.AgentTurnSettlementObservation {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, diagnostic, request.RepositoryCredential)
	}
	proposalScope := request.Execution.ChangeProposal
	if strings.TrimSpace(request.RepositoryCredential) == "" || proposalScope == nil {
		return failure("Reviewer repository credential or Change Proposal is unavailable")
	}
	var arguments struct {
		OperationID string                `json:"operation_id"`
		Event       githubapi.ReviewEvent `json:"event"`
		Body        string                `json:"body"`
		Comments    json.RawMessage       `json:"comments"`
	}
	var result struct {
		ReviewID int64  `json:"review_id"`
		NodeID   string `json:"node_id"`
		State    string `json:"state"`
		CommitID string `json:"commit_id"`
		ActorID  int64  `json:"actor_id"`
		HTMLURL  string `json:"html_url"`
	}
	expectedState := ""
	if !decodeExactObject(intent.Request, &arguments) {
		return failure("submit_review request evidence is malformed")
	}
	switch arguments.Event {
	case githubapi.ReviewApprove:
		expectedState = "APPROVED"
	case githubapi.ReviewRequestChanges:
		expectedState = "CHANGES_REQUESTED"
	default:
		return failure("submit_review request has an invalid review state")
	}
	if intent.ExternalService != "github" || !exactNumericResource(intent.ExternalResourceID, request.Execution.Repository.ID, proposalScope.PullRequestID) ||
		arguments.OperationID != intent.OperationID || intent.ExpectedSHA != request.Execution.Turn.ExpectedHeadSHA ||
		!decodeExactObject(intent.Result, &result) || result.ReviewID <= 0 || strings.TrimSpace(result.NodeID) == "" ||
		result.ActorID <= 0 || result.State != expectedState || result.CommitID != request.Execution.Turn.ExpectedHeadSHA {
		return failure("submit_review terminal evidence is malformed or incoherent")
	}

	pullRequest, err := reconciler.github.GetPullRequest(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(proposalScope.PullRequestNumber))
	if err != nil {
		return failure("fresh Reviewer Pull Request observation failed")
	}
	if !matchesReviewerPullRequest(pullRequest, request.Execution.Repository, *proposalScope) {
		return failure("fresh Reviewer Pull Request does not match the durable Change Proposal")
	}
	reviews, err := reconciler.github.ListPullRequestReviews(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(proposalScope.PullRequestNumber))
	if err != nil {
		return failure("fresh Pull Request review observation failed")
	}
	matched := 0
	for _, review := range reviews {
		if review.ID == result.ReviewID && review.NodeID == result.NodeID && review.State == result.State &&
			review.CommitID == result.CommitID && review.User.ID == result.ActorID {
			matched++
		}
	}
	if matched != 1 {
		return failure("submitted review identity is absent or conflicts with fresh GitHub state")
	}
	reviewIdentity := &workflow.ReviewIdentity{
		ID: result.ReviewID, NodeID: result.NodeID, ChangeProposalID: proposalScope.PullRequestID,
		ActorID: result.ActorID, HeadSHA: result.CommitID,
	}
	if containsCredentialInReview(reviewIdentity, request.RepositoryCredential) {
		return failure("submitted review identity contains unsafe credential material")
	}
	existing, err := reconciler.store.GetChangeProposalReview(ctx, request.Execution.Repository.ID, result.ReviewID)
	if err != nil {
		return failure("durable submitted review lookup failed")
	}
	proposal := settlementChangeProposal(request.Execution, pullRequest)
	if containsCredentialInProposal(proposal, request.RepositoryCredential) || containsCredentialInReview(existing, request.RepositoryCredential) {
		return failure("review reconciliation contains unsafe credential material")
	}
	outcome := workflow.TurnOutcomeApproved
	if result.State == "CHANGES_REQUESTED" {
		outcome = workflow.TurnOutcomeChangesRequested
	}
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: outcome, ChangeProposal: proposal, Review: reviewIdentity,
		ExistingReview: existing, AuthorizedReviewerActorID: result.ActorID,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: promptOutcome},
	}
}

type openPullRequestEvidence struct {
	PullRequestID     int64
	PullRequestNumber int64
	NodeID            string
	HeadSHA           string
	HeadRef           string
	BaseRef           string
}

func parseOpenPullRequestEvidence(mutation store.MutationReservation, repositoryID int64) (openPullRequestEvidence, bool) {
	var arguments struct {
		OperationID string `json:"operation_id"`
		Title       string `json:"title"`
		Body        string `json:"body"`
	}
	var result struct {
		PullRequestID int64  `json:"pull_request_id"`
		NodeID        string `json:"node_id"`
		Number        int64  `json:"number"`
		HTMLURL       string `json:"html_url"`
		HeadSHA       string `json:"head_sha"`
	}
	head, base, resourceOK := exactOpenPullRequestResource(mutation.ExternalResourceID, repositoryID)
	if mutation.ExternalService != "github" || !resourceOK || !decodeExactObject(mutation.Request, &arguments) ||
		arguments.OperationID != mutation.OperationID || strings.TrimSpace(arguments.Title) == "" ||
		!decodeExactObject(mutation.Result, &result) || result.PullRequestID <= 0 || result.Number <= 0 ||
		strings.TrimSpace(result.NodeID) == "" || strings.TrimSpace(result.HeadSHA) == "" || mutation.ExpectedSHA != result.HeadSHA {
		return openPullRequestEvidence{}, false
	}
	return openPullRequestEvidence{
		PullRequestID: result.PullRequestID, PullRequestNumber: result.Number, NodeID: result.NodeID,
		HeadSHA: result.HeadSHA, HeadRef: head, BaseRef: base,
	}, true
}

func blockedObservation(observedAt time.Time, promptOutcome json.RawMessage, mutation store.MutationReservation, workflowID, credential string) store.AgentTurnSettlementObservation {
	var arguments struct {
		OperationID string `json:"operation_id"`
		Reason      string `json:"reason"`
		Details     string `json:"details"`
	}
	var result struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Details string `json:"details"`
	}
	if mutation.ExternalService != "omnigrex" || mutation.ExternalResourceID != workflowID || mutation.ExpectedSHA != "" ||
		!decodeExactObject(mutation.Request, &arguments) || arguments.OperationID != mutation.OperationID ||
		!decodeExactObject(mutation.Result, &result) || result.Outcome != "BLOCKED" ||
		strings.TrimSpace(result.Reason) == "" || result.Reason != arguments.Reason || result.Details != arguments.Details {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "report_blocked terminal evidence is malformed or incoherent", credential)
	}
	reason := sanitizeOutcomeDiagnostic(result.Reason, credential)
	details := sanitizeOutcomeDiagnostic(result.Details, credential)
	if reason == "" {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "report_blocked terminal evidence has no safe diagnostic", credential)
	}
	diagnostic := reason
	if details != "" {
		diagnostic += ": " + details
	}
	diagnostic = sanitizeOutcomeDiagnostic(diagnostic, credential)
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeBlocked, Diagnostic: diagnostic,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: promptOutcome},
	}
}

func validOutcomeBinding(lease store.AgentTurnLease, execution store.AgentTurnExecutionContext) bool {
	return lease.ID != "" && lease.ExecutionEpoch > 0 && lease.ControlRevision > 0 &&
		execution.WorkflowID != "" && execution.WorkflowID == lease.JobLease.WorkflowID &&
		execution.Repository.ID > 0 && strings.TrimSpace(execution.Repository.Owner) != "" && strings.TrimSpace(execution.Repository.Name) != "" &&
		execution.Assignment.ID == lease.AgentAssignmentID && execution.Assignment.WorkflowID == execution.WorkflowID &&
		execution.Session.ID == lease.AgentSessionID && execution.Session.AgentAssignmentID == execution.Assignment.ID &&
		execution.Turn.ID == lease.ID && execution.Turn.AgentAssignmentID == lease.AgentAssignmentID &&
		execution.Turn.AgentSessionID == lease.AgentSessionID && execution.Turn.ExecutionEpoch == lease.ExecutionEpoch &&
		execution.Turn.ControlRevision == lease.ControlRevision &&
		(execution.Assignment.Role == workflow.RoleDeveloper || execution.Assignment.Role == workflow.RoleReviewer)
}

func validPromptInput(request OutcomeReconciliation) bool {
	hasResponse := request.PromptResponse != nil
	hasError := request.PromptError != ""
	if hasResponse == hasError {
		return false
	}
	if !hasError {
		return true
	}
	return request.PromptError == PromptErrorDeadline || request.PromptError == PromptErrorCancellation || request.PromptError == PromptErrorFailure
}

func validateTerminalLedger(mutations []store.MutationReservation, lease store.AgentTurnLease) string {
	var previous int64
	for _, mutation := range mutations {
		if mutation.AgentTurnID != lease.ID || mutation.ExecutionEpoch != lease.ExecutionEpoch || mutation.InvocationNumber <= previous ||
			mutation.InvocationNumber <= 0 || strings.TrimSpace(mutation.ID) == "" || strings.TrimSpace(mutation.OperationID) == "" ||
			strings.TrimSpace(mutation.ToolName) == "" || mutation.AdmittedAt.IsZero() || mutation.FinishedAt == nil ||
			!jsonObject(mutation.Request) {
			return "mutation ledger contains malformed invocation metadata"
		}
		previous = mutation.InvocationNumber
		switch mutation.State {
		case store.MutationSucceeded:
			if !jsonObject(mutation.Result) || mutation.LastError != "" {
				return "mutation ledger contains malformed successful evidence"
			}
		case store.MutationFailed:
			if len(mutation.Result) != 0 || strings.TrimSpace(mutation.LastError) == "" {
				return "mutation ledger contains malformed failed evidence"
			}
		default:
			return "mutation ledger contains an unsettled mutation"
		}
	}
	return ""
}

func ledgerHasUnsettledMutation(mutations []store.MutationReservation) bool {
	for _, mutation := range mutations {
		switch mutation.State {
		case store.MutationReserved, store.MutationInFlight, store.MutationUnknown, store.MutationReconciling:
			return true
		}
	}
	return false
}

func successfulTerminalIntents(mutations []store.MutationReservation) []store.MutationReservation {
	intents := make([]store.MutationReservation, 0, 1)
	for _, mutation := range mutations {
		if mutation.State != store.MutationSucceeded {
			continue
		}
		switch mutation.ToolName {
		case mcp.ToolReportBlocked, mcp.ToolRequestReview, mcp.ToolSubmitReview:
			intents = append(intents, mutation)
		}
	}
	return intents
}

func promptFailureDiagnostic(request OutcomeReconciliation) string {
	if request.PromptError != "" {
		diagnostic := "ACP prompt failed"
		switch request.PromptError {
		case PromptErrorDeadline:
			diagnostic = "ACP prompt deadline exceeded"
		case PromptErrorCancellation:
			diagnostic = "ACP prompt was cancelled"
		}
		if strings.TrimSpace(request.PromptDiagnostic) != "" {
			diagnostic += ": " + request.PromptDiagnostic
		}
		return diagnostic
	}
	switch request.PromptResponse.StopReason {
	case acp.StopReasonEndTurn:
		return ""
	case acp.StopReasonMaxTokens:
		return "ACP prompt reached its token limit"
	case acp.StopReasonMaxTurnRequests:
		return "ACP prompt reached its request limit"
	case acp.StopReasonRefusal:
		return "ACP prompt was refused"
	case acp.StopReasonCancelled:
		return "ACP prompt was cancelled"
	default:
		return "ACP prompt returned an invalid stop reason"
	}
}

func promptTerminalStatus(request OutcomeReconciliation) store.AgentTurnStatus {
	if request.PromptError == PromptErrorDeadline {
		return store.AgentTurnTimedOut
	}
	if request.PromptError == PromptErrorCancellation || request.PromptResponse != nil && request.PromptResponse.StopReason == acp.StopReasonCancelled {
		return store.AgentTurnInterrupted
	}
	return store.AgentTurnFailed
}

func encodedPromptOutcome(response *acp.PromptResponse) json.RawMessage {
	if response == nil {
		return nil
	}
	encoded, _ := json.Marshal(struct {
		StopReason acp.StopReason `json:"stop_reason"`
	}{response.StopReason})
	return encoded
}

func infrastructureObservation(observedAt time.Time, status store.AgentTurnStatus, promptOutcome json.RawMessage, diagnostic, credential string) store.AgentTurnSettlementObservation {
	diagnostic = sanitizeOutcomeDiagnostic(diagnostic, credential)
	if diagnostic == "" {
		diagnostic = "Agent Turn outcome reconciliation failed"
	}
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeInfrastructureFailed, Diagnostic: diagnostic,
		Completion: store.AgentTurnCompletion{Status: status, Outcome: promptOutcome, LastError: diagnostic},
	}
}

func sanitizeOutcomeDiagnostic(value, credential string) string {
	if credential != "" {
		value = strings.ReplaceAll(value, credential, "[REDACTED]")
	}
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxOutcomeDiagnosticRunes {
		value = string(runes[:maxOutcomeDiagnosticRunes])
	}
	return value
}

func decodeExactObject(raw json.RawMessage, target any) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(&struct{}{}) == io.EOF
}

func jsonObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func exactNumericResource(resource string, repositoryID, resourceID int64) bool {
	want := strconv.FormatInt(repositoryID, 10) + ":" + strconv.FormatInt(resourceID, 10)
	return resource == want
}

func exactBranchResource(resource string, repositoryID int64) (string, bool) {
	prefix := strconv.FormatInt(repositoryID, 10) + ":"
	branch := strings.TrimPrefix(resource, prefix)
	return branch, strings.HasPrefix(resource, prefix) && validObservedRef(branch)
}

func exactOpenPullRequestResource(resource string, repositoryID int64) (string, string, bool) {
	prefix := strconv.FormatInt(repositoryID, 10) + ":"
	refs := strings.TrimPrefix(resource, prefix)
	head, base, found := strings.Cut(refs, ":")
	return head, base, strings.HasPrefix(resource, prefix) && found && !strings.Contains(base, ":") && validObservedRef(head) && validObservedRef(base) && head != base
}

func validObservedRef(value string) bool {
	if strings.TrimSpace(value) != value || value == "" || value == "@" || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") ||
		strings.Contains(value, "@{") || strings.Contains(value, "//") || strings.ContainsAny(value, "~^:?*[\\\x00") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func matchesDeveloperPullRequest(pullRequest githubapi.PullRequest, repository store.AgentTurnRepository, result struct {
	Outcome           string `json:"outcome"`
	PullRequestID     int64  `json:"pull_request_id"`
	PullRequestNumber int64  `json:"pull_request_number"`
	HeadSHA           string `json:"head_sha"`
}, branch, base string) bool {
	return pullRequest.ID == result.PullRequestID && int64(pullRequest.Number) == result.PullRequestNumber &&
		strings.EqualFold(pullRequest.State, "open") && !pullRequest.Merged && strings.TrimSpace(pullRequest.NodeID) != "" &&
		pullRequest.Head.Ref == branch && pullRequest.Head.SHA == result.HeadSHA && pullRequest.Head.Label == repository.Owner+":"+branch &&
		pullRequest.Base.Ref == base && strings.TrimSpace(pullRequest.Base.SHA) != "" && pullRequest.Base.Label == repository.Owner+":"+base
}

func matchesReviewerPullRequest(pullRequest githubapi.PullRequest, repository store.AgentTurnRepository, proposal store.AgentTurnChangeProposal) bool {
	return pullRequest.ID == proposal.PullRequestID && int64(pullRequest.Number) == proposal.PullRequestNumber &&
		strings.EqualFold(pullRequest.State, "open") && !pullRequest.Merged && strings.TrimSpace(pullRequest.NodeID) != "" &&
		pullRequest.Head.Ref == proposal.HeadRef && strings.TrimSpace(pullRequest.Head.SHA) != "" && pullRequest.Head.Label == repository.Owner+":"+proposal.HeadRef &&
		pullRequest.Base.Ref == proposal.BaseRef && strings.TrimSpace(pullRequest.Base.SHA) != "" && pullRequest.Base.Label == repository.Owner+":"+proposal.BaseRef
}

func settlementChangeProposal(execution store.AgentTurnExecutionContext, pullRequest githubapi.PullRequest) *store.AgentTurnSettlementChangeProposal {
	return &store.AgentTurnSettlementChangeProposal{
		WorkflowID: execution.WorkflowID, RepositoryID: execution.Repository.ID,
		RepositoryOwner: execution.Repository.Owner, RepositoryName: execution.Repository.Name,
		PullRequestID: pullRequest.ID, PullRequestNumber: int64(pullRequest.Number), PullRequestNodeID: pullRequest.NodeID,
		Status: "OPEN", Active: true, BaseRef: pullRequest.Base.Ref, BaseSHA: pullRequest.Base.SHA,
		HeadRef: pullRequest.Head.Ref, HeadSHA: pullRequest.Head.SHA,
	}
}

func containsCredentialInProposal(proposal *store.AgentTurnSettlementChangeProposal, credential string) bool {
	if proposal == nil || credential == "" {
		return false
	}
	return containsCredential(proposal.WorkflowID, credential) || containsCredential(proposal.RepositoryOwner, credential) ||
		containsCredential(proposal.RepositoryName, credential) || containsCredential(proposal.PullRequestNodeID, credential) ||
		containsCredential(proposal.BaseRef, credential) || containsCredential(proposal.BaseSHA, credential) ||
		containsCredential(proposal.HeadRef, credential) || containsCredential(proposal.HeadSHA, credential)
}

func containsCredentialInReview(review *workflow.ReviewIdentity, credential string) bool {
	return review != nil && (containsCredential(review.NodeID, credential) || containsCredential(review.HeadSHA, credential))
}

func containsCredential(value, credential string) bool {
	return credential != "" && strings.Contains(value, credential)
}
