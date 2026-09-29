package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
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
	PromptErrorDeadline        PromptErrorClassification = "DEADLINE"
	PromptErrorCancellation    PromptErrorClassification = "CANCELLATION"
	PromptErrorFailure         PromptErrorClassification = "FAILURE"
	PromptErrorInvalidResponse PromptErrorClassification = "INVALID_RESPONSE"
)

// ClassifyPromptError maps ACP prompt errors to the terminal Agent Turn status contract.
func ClassifyPromptError(err error) PromptErrorClassification {
	if errors.Is(err, ErrRuntimeOOMKilled) {
		return PromptErrorFailure
	}
	if errors.Is(err, acp.ErrUnknownStopReason) {
		return PromptErrorInvalidResponse
	}
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
	ListParticipantPublicationMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error)
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
	Store                  OutcomeReconcilerStore
	GitHub                 OutcomeReconcilerGitHub
	Clock                  interface{ Now() time.Time }
	Logger                 *slog.Logger
	ProviderCredentialJSON []json.RawMessage
}

func (OutcomeReconcilerConfig) String() string { return "Agent Turn outcome reconciler config" }
func (OutcomeReconcilerConfig) GoString() string {
	return "agentturn.OutcomeReconcilerConfig{<credentials redacted>}"
}

// OutcomeReconciliation contains the exact acquired turn context and ephemeral operation credentials.
// Exactly one of PromptResponse and PromptError must be supplied.
type OutcomeReconciliation struct {
	Lease                        store.AgentTurnLease
	Execution                    store.AgentTurnExecutionContext
	PromptResponse               *acp.PromptResponse
	PromptError                  PromptErrorClassification
	PromptDiagnostic             string
	RepositoryCredential         string
	ReviewerRepositoryCredential string
	Paths                        workspace.Paths
	PriorPublicationMutations    []store.MutationReservation
	OnCorroborationFailure       func(TerminalCorroborationFailure)
}

// TerminalCorroborationFailure describes an unavailable observation without
// carrying the dependency's potentially credential-bearing error.
type TerminalCorroborationFailure struct {
	SourceInvocationID string
	Code               string
	Retryable          bool
	Prerequisite       bool
	RetryAfter         time.Duration
}

func (OutcomeReconciliation) String() string { return "Agent Turn outcome reconciliation" }
func (OutcomeReconciliation) GoString() string {
	return "agentturn.OutcomeReconciliation{<credentials redacted>}"
}

// OutcomeReconciler derives settlement observations without consulting ACP output text.
type OutcomeReconciler struct {
	store              OutcomeReconcilerStore
	github             OutcomeReconcilerGitHub
	clock              outcomeReconcilerClock
	logger             *slog.Logger
	diagnosticRedactor *strings.Replacer
}

func (*OutcomeReconciler) String() string { return "Agent Turn outcome reconciler" }
func (*OutcomeReconciler) GoString() string {
	return "agentturn.OutcomeReconciler{<credentials redacted>}"
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
	redactor, err := newProviderDiagnosticRedactor(config.ProviderCredentialJSON)
	if err != nil {
		return nil, ErrInvalidOutcomeReconciler
	}
	return &OutcomeReconciler{store: config.Store, github: config.GitHub, clock: clock, logger: config.Logger, diagnosticRedactor: redactor}, nil
}

// Reconcile reads the closed terminal ledger and corroborates its sole successful terminal intent.
func (reconciler *OutcomeReconciler) Reconcile(ctx context.Context, request OutcomeReconciliation) (observation store.AgentTurnSettlementObservation, err error) {
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
	return reconciler.ReconcileRecorded(ctx, request, mutations)
}

// ReconcileRecorded uses a verifier-owned snapshot of an already closed,
// settled mutation ledger. It does not obtain or confer a live Agent Turn
// lease; the Store must fence its source before providing these records.
func (reconciler *OutcomeReconciler) ReconcileRecorded(ctx context.Context, request OutcomeReconciliation, mutations []store.MutationReservation) (observation store.AgentTurnSettlementObservation, err error) {
	if reconciler == nil || !validOutcomeBinding(request.Lease, request.Execution) || !validPromptInput(request) {
		return store.AgentTurnSettlementObservation{}, ErrInvalidOutcomeReconciliation
	}
	defer func() {
		observation = reconciler.sanitizeObservation(observation, request.RepositoryCredential)
		observation = reconciler.sanitizeObservation(observation, request.ReviewerRepositoryCredential)
	}()
	observedAt := reconciler.clock.Now().UTC()
	if observedAt.IsZero() {
		return store.AgentTurnSettlementObservation{}, ErrInvalidOutcomeReconciler
	}
	promptOutcome := encodedPromptOutcome(request.PromptResponse)
	if ledgerHasUnsettledMutation(mutations) {
		return store.AgentTurnSettlementObservation{}, store.ErrAgentTurnMutationsUnsettled
	}
	if diagnostic := validateTerminalLedger(mutations, request.Lease); diagnostic != "" {
		return infrastructureObservation(observedAt, promptTerminalStatus(request), promptOutcome, diagnostic), nil
	}
	intents := successfulTerminalIntents(mutations)
	if len(intents) > 1 {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "mutation ledger contains duplicate or conflicting terminal intents"), nil
	}
	promptDiagnostic := promptFailureDiagnostic(request)
	// A lost response or deadline does not invalidate a completed terminal intent.
	// Explicit cancellation and non-normal ACP stop reasons still prevent acceptance.
	if promptDiagnostic != "" && request.PromptError != PromptErrorFailure && request.PromptError != PromptErrorDeadline {
		return infrastructureObservation(observedAt, promptTerminalStatus(request), promptOutcome, promptDiagnostic), nil
	}
	if len(intents) == 0 {
		if publication := request.Execution.Publication; publication != nil {
			for _, mutation := range mutations {
				if mutation.State == store.MutationFailed && mutation.ToolName == mcp.ToolPublishChanges &&
					(mutation.LastError == mcp.FailurePublicationRemoteHeadMismatch ||
						strings.HasPrefix(mutation.LastError, mcp.FailurePublicationRemoteHeadMismatch+" ")) {
					return publicationConflictObservation(observedAt, promptOutcome), nil
				}
				if mutation.State == store.MutationFailed && mutation.ToolName == mcp.ToolRequestReview &&
					mutation.LastError == mcp.FailurePullRequestHeadMismatch &&
					(publication.PullRequestID > 0 || openedPRForRecoveredBranch(mutations, *publication, request.Execution.Repository.ID)) {
					return publicationConflictObservation(observedAt, promptOutcome), nil
				}
			}
		}
		if promptDiagnostic != "" {
			return infrastructureObservation(observedAt, promptTerminalStatus(request), promptOutcome, promptDiagnostic), nil
		}
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "ACP prompt ended without a successful terminal mutation intent"), nil
	}
	intent := intents[0]
	var result store.AgentTurnSettlementObservation
	if intent.ToolName == mcp.ToolReportBlocked {
		result = blockedObservation(observedAt, promptOutcome, intent, request.Execution.WorkflowID)
	} else {
		switch intent.ToolName {
		case mcp.ToolRequestReview:
			result = reconciler.reconcileDeveloper(ctx, request, observedAt, promptOutcome, mutations, intent)
		case mcp.ToolSubmitReview:
			request.RepositoryCredential = request.ReviewerRepositoryCredential
			result = reconciler.reconcileReviewer(ctx, request, observedAt, promptOutcome, intent)
		case mcp.ToolConfirmPriorTerminalIntent:
			var source struct {
				SourceInvocationID string          `json:"source_invocation_id"`
				SourceTool         string          `json:"source_tool"`
				SourceOperationID  string          `json:"source_operation_id"`
				SourceRequest      json.RawMessage `json:"source_request"`
				SourceResult       json.RawMessage `json:"source_result"`
				ExternalService    string          `json:"external_service"`
				ExternalResourceID string          `json:"external_resource_id"`
				ExpectedSHA        string          `json:"expected_sha"`
			}
			var confirmRequest struct {
				OperationID        string `json:"operation_id"`
				SourceInvocationID string `json:"source_invocation_id"`
			}
			if !decodeExactObject(intent.Request, &confirmRequest) || !decodeExactObject(intent.Result, &source) ||
				confirmRequest.OperationID != intent.OperationID || confirmRequest.SourceInvocationID != source.SourceInvocationID ||
				source.SourceInvocationID == "" || source.SourceOperationID == "" || !jsonObject(source.SourceRequest) ||
				!jsonObject(source.SourceResult) || intent.ExternalService != "omnigrex" ||
				intent.ExternalResourceID != request.Execution.WorkflowID || intent.ExpectedSHA != source.ExpectedSHA {
				return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "confirmed terminal evidence is malformed or incoherent"), nil
			}
			prior := intent
			prior.ID, prior.OperationID, prior.ToolName = source.SourceInvocationID, source.SourceOperationID, source.SourceTool
			prior.Request, prior.Result = source.SourceRequest, source.SourceResult
			prior.ExternalService, prior.ExternalResourceID, prior.ExpectedSHA = source.ExternalService, source.ExternalResourceID, source.ExpectedSHA
			// The prior mutation supplies evidence, but the terminal intent whose
			// outcome is pending belongs to this Turn's confirmation invocation.
			if original := request.OnCorroborationFailure; original != nil {
				request.OnCorroborationFailure = func(failure TerminalCorroborationFailure) {
					failure.SourceInvocationID = intent.ID
					original(failure)
				}
			}
			switch prior.ToolName {
			case mcp.ToolRequestReview:
				if request.Execution.Assignment.Role != workflow.RoleDeveloper {
					return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "confirmed terminal Role does not match source"), nil
				}
				result = reconciler.reconcileDeveloper(ctx, request, observedAt, promptOutcome, mutations, prior)
			case mcp.ToolSubmitReview:
				if request.Execution.Assignment.Role != workflow.RoleReviewer {
					return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "confirmed terminal Role does not match source"), nil
				}
				request.RepositoryCredential = request.ReviewerRepositoryCredential
				result = reconciler.reconcileReviewer(ctx, request, observedAt, promptOutcome, prior)
			default:
				return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "confirmed terminal source kind is invalid"), nil
			}
		default:
			return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "mutation ledger contains an unsupported terminal evidence kind"), nil
		}
	}
	if result.Outcome == workflow.TurnOutcomeInfrastructureFailed && request.PromptError != "" {
		result.Completion.Status = promptTerminalStatus(request)
	} else if result.Outcome != workflow.TurnOutcomeBlocked && request.PromptError != "" {
		result.Diagnostic = promptDiagnostic
	}
	return result, nil
}

func openedPRForRecoveredBranch(mutations []store.MutationReservation, publication store.AgentTurnPublication, repositoryID int64) bool {
	var opened int
	for _, mutation := range mutations {
		if mutation.State != store.MutationSucceeded || mutation.ToolName != mcp.ToolOpenPR {
			continue
		}
		evidence, ok := parseOpenPullRequestEvidence(mutation, repositoryID)
		if !ok || evidence.HeadRef != publication.HeadRef || evidence.BaseRef != publication.BaseRef {
			return false
		}
		opened++
	}
	return opened == 1
}

func newProviderDiagnosticRedactor(providers []json.RawMessage) (*strings.Replacer, error) {
	values := make(map[string]struct{})
	add := func(value string) {
		if value == "" {
			return
		}
		values[value] = struct{}{}
		quoted, _ := json.Marshal(value)
		values[string(quoted)] = struct{}{}
		if len(quoted) >= 2 {
			values[string(quoted[1:len(quoted)-1])] = struct{}{}
		}
	}
	for _, provider := range providers {
		var decoded any
		if json.Unmarshal(provider, &decoded) != nil {
			return nil, ErrInvalidOutcomeReconciler
		}
		if _, ok := decoded.(map[string]any); !ok {
			return nil, ErrInvalidOutcomeReconciler
		}
		add(string(provider))
		canonical, _ := json.Marshal(decoded)
		add(string(canonical))
		// OpenCode auth secrets are strings; its accepted numeric expires field is metadata.
		// Redacting standalone JSON scalars would erase ordinary diagnostics such as 1, true, or null.
		leaves := make([]string, 0)
		collectStringSecrets(decoded, &leaves)
		for _, leaf := range leaves {
			add(leaf)
		}
	}
	sensitive := make([]string, 0, len(values))
	for value := range values {
		sensitive = append(sensitive, value)
	}
	sort.Slice(sensitive, func(left, right int) bool {
		return len(sensitive[left]) > len(sensitive[right])
	})
	replacements := make([]string, 0, len(sensitive)*2)
	for _, value := range sensitive {
		replacements = append(replacements, value, "[REDACTED]")
	}
	return strings.NewReplacer(replacements...), nil
}

func (reconciler *OutcomeReconciler) redactProviderDiagnostic(value string) string {
	if reconciler.diagnosticRedactor == nil {
		return value
	}
	return reconciler.diagnosticRedactor.Replace(value)
}

func (reconciler *OutcomeReconciler) reconcileDeveloper(ctx context.Context, request OutcomeReconciliation, observedAt time.Time, promptOutcome json.RawMessage, mutations []store.MutationReservation, intent store.MutationReservation) store.AgentTurnSettlementObservation {
	failure := func(diagnostic string) store.AgentTurnSettlementObservation {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, diagnostic)
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
	} else if opened != nil && (opened.PullRequestID != result.PullRequestID || opened.PullRequestNumber != result.PullRequestNumber || opened.HeadRef != branch) {
		return failure("request_review evidence conflicts with open_pr evidence")
	}
	if publication := request.Execution.Publication; publication != nil && publication.PullRequestID > 0 {
		if opened != nil || publication.PullRequestID != result.PullRequestID || publication.PullRequestNumber != result.PullRequestNumber ||
			publication.HeadRef != branch || publication.BaseRef == "" || publication.SourceOpenPRMutationID == "" {
			return failure("request_review evidence conflicts with recovered publication")
		}
		baseRef = publication.BaseRef
	}
	provenOpen := opened
	if publication := request.Execution.Publication; publication != nil && publication.PullRequestID > 0 {
		prior := request.PriorPublicationMutations
		if prior == nil {
			var err error
			prior, err = reconciler.store.ListParticipantPublicationMutations(ctx, request.Lease)
			if err != nil {
				reportTerminalCorroborationFailure(request, intent.ID,
					corroborationFailure{code: "database_observation_unavailable", retryable: true})
				return failure("recovered Pull Request publication evidence is unavailable")
			}
		}
		for _, mutation := range prior {
			if mutation.ID != publication.SourceOpenPRMutationID {
				continue
			}
			if provenOpen != nil {
				return failure("recovered Pull Request publication evidence is ambiguous")
			}
			evidence, ok := parseOpenPullRequestEvidence(mutation, request.Execution.Repository.ID)
			if !ok || evidence.PullRequestID != publication.PullRequestID || evidence.PullRequestNumber != publication.PullRequestNumber ||
				evidence.NodeID != publication.PullRequestNodeID || evidence.HeadRef != publication.HeadRef || evidence.BaseRef != publication.BaseRef {
				return failure("recovered Pull Request publication evidence is malformed or incoherent")
			}
			provenOpen = &evidence
		}
		if provenOpen == nil {
			return failure("recovered Pull Request publication evidence is unavailable")
		}
	}

	pullRequest, err := reconciler.getPullRequest(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(result.PullRequestNumber))
	if err != nil {
		reconciler.logGitHubObservationFailure(request, "get_pull_request", result.PullRequestNumber, err)
		reportGitHubCorroborationFailure(request, intent.ID, err)
		return failure("fresh Developer Pull Request observation failed")
	}
	if baseRef == "" {
		baseRef = pullRequest.Base.Ref
	}
	if provenOpen != nil {
		marker, err := githubapi.RenderMarker(githubapi.Marker{
			WorkflowID: request.Execution.WorkflowID, AgentAssignmentID: request.Execution.Assignment.ID,
			OperationID: provenOpen.MutationID,
		})
		if err != nil || pullRequest.NodeID != provenOpen.NodeID || pullRequest.Title != provenOpen.Title ||
			pullRequest.Body != githubapi.JoinBodyParts(provenOpen.Body, fmt.Sprintf("Closes #%d", request.Execution.Issue.Number), marker) {
			return publicationConflictObservation(observedAt, promptOutcome)
		}
	}
	if !matchesDeveloperPullRequest(pullRequest, request.Execution.Repository, result, branch, baseRef) {
		if provenOpen != nil {
			return publicationConflictObservation(observedAt, promptOutcome)
		}
		return failure("fresh Developer Pull Request does not match terminal evidence")
	}
	if opened != nil && (opened.NodeID != pullRequest.NodeID || opened.PullRequestID != pullRequest.ID || opened.PullRequestNumber != int64(pullRequest.Number)) {
		return failure("fresh Developer Pull Request conflicts with open_pr identity")
	}
	proposal := settlementChangeProposal(request.Execution, pullRequest)
	if containsCredentialInProposal(proposal, request.RepositoryCredential) {
		return failure("fresh Developer Pull Request contains unsafe credential material")
	}
	matching, err := workspace.CommittedTreesEqual(ctx, request.Paths)
	if err != nil {
		reportTerminalCorroborationFailure(request, intent.ID, corroborationFailure{code: "workspace_observation_unavailable", retryable: true})
		return failure("Developer committed workspace observation failed")
	}
	if !matching {
		return failure("Developer workspace has unpublished committed changes")
	}
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeChangeProposalReady, ChangeProposal: proposal,
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: promptOutcome},
	}
}

// A recovered PR is authorized only while its original identity and exact
// publication remain corroborated. A definite fresh conflict is a blocker,
// not an infrastructure failure that should silently retry.
func publicationConflictObservation(observedAt time.Time, promptOutcome json.RawMessage) store.AgentTurnSettlementObservation {
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeBlocked,
		Diagnostic: "publication_conflict: recovered Pull Request changed during Developer turn",
		Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded, Outcome: promptOutcome},
	}
}

func (reconciler *OutcomeReconciler) reconcileReviewer(ctx context.Context, request OutcomeReconciliation, observedAt time.Time, promptOutcome json.RawMessage, intent store.MutationReservation) store.AgentTurnSettlementObservation {
	failure := func(diagnostic string) store.AgentTurnSettlementObservation {
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, diagnostic)
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
		Signature   json.RawMessage       `json:"signature"`
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
	// An absent signature is valid for legacy reservations, but an explicit null is not a string.
	if len(arguments.Signature) > 0 {
		var signature *string
		if json.Unmarshal(arguments.Signature, &signature) != nil || signature == nil {
			return failure("submit_review request evidence is malformed")
		}
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

	pullRequest, err := reconciler.getPullRequest(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(proposalScope.PullRequestNumber))
	if err != nil {
		reconciler.logGitHubObservationFailure(request, "get_pull_request", proposalScope.PullRequestNumber, err)
		reportGitHubCorroborationFailure(request, intent.ID, err)
		return failure("fresh Reviewer Pull Request observation failed")
	}
	if !matchesReviewerPullRequest(pullRequest, request.Execution.Repository, *proposalScope) {
		return failure("fresh Reviewer Pull Request does not match the durable Change Proposal")
	}
	reviews, err := reconciler.listPullRequestReviews(ctx, request.RepositoryCredential, request.Execution.Repository.Owner,
		request.Execution.Repository.Name, int(proposalScope.PullRequestNumber))
	if err != nil {
		reconciler.logGitHubObservationFailure(request, "list_pull_request_reviews", proposalScope.PullRequestNumber, err)
		reportGitHubCorroborationFailure(request, intent.ID, err)
		return failure("fresh Pull Request review observation failed")
	}
	matched := 0
	seenReviewID := false
	for _, review := range reviews {
		if review.ID == result.ReviewID {
			seenReviewID = true
		}
		if review.ID == result.ReviewID && review.NodeID == result.NodeID && review.State == result.State &&
			review.CommitID == result.CommitID && review.User.ID == result.ActorID {
			matched++
		}
	}
	if matched != 1 {
		if !seenReviewID {
			reportTerminalCorroborationFailure(request, intent.ID,
				corroborationFailure{code: "review_not_visible_yet", retryable: true})
		}
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
		reportTerminalCorroborationFailure(request, intent.ID,
			corroborationFailure{code: "database_observation_unavailable", retryable: true})
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

func reportTerminalCorroborationFailure(request OutcomeReconciliation, sourceID string, failure corroborationFailure) {
	if request.OnCorroborationFailure != nil {
		request.OnCorroborationFailure(TerminalCorroborationFailure{
			SourceInvocationID: sourceID, Code: failure.code,
			Retryable: failure.retryable, Prerequisite: failure.prerequisite,
		})
	}
}

func reportGitHubCorroborationFailure(request OutcomeReconciliation, sourceID string, err error) {
	if request.OnCorroborationFailure == nil {
		return
	}
	failure := classifyGitHubCorroborationFailure(err)
	report := TerminalCorroborationFailure{
		SourceInvocationID: sourceID, Code: failure.code,
		Retryable: failure.retryable, Prerequisite: failure.prerequisite,
	}
	var rateLimit *githubapi.RateLimitError
	if errors.As(err, &rateLimit) {
		report.RetryAfter = rateLimit.RetryAfter
		if untilReset := time.Until(rateLimit.ResetAt); untilReset > report.RetryAfter {
			report.RetryAfter = untilReset
		}
	}
	request.OnCorroborationFailure(report)
}

// Short-lived read failures are retried while the original Turn still owns its
// settlement fence. This does not replace durable corroboration across restarts.
func (reconciler *OutcomeReconciler) getPullRequest(ctx context.Context, credential, owner, repository string, number int) (githubapi.PullRequest, error) {
	var pullRequest githubapi.PullRequest
	err := retryTransientObservation(ctx, func() error {
		var err error
		pullRequest, err = reconciler.github.GetPullRequest(ctx, credential, owner, repository, number)
		return err
	})
	return pullRequest, err
}

func (reconciler *OutcomeReconciler) listPullRequestReviews(ctx context.Context, credential, owner, repository string, number int) ([]githubapi.Review, error) {
	var reviews []githubapi.Review
	err := retryTransientObservation(ctx, func() error {
		var err error
		reviews, err = reconciler.github.ListPullRequestReviews(ctx, credential, owner, repository, number)
		return err
	})
	return reviews, err
}

func retryTransientObservation(ctx context.Context, observe func() error) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := observe()
		if err == nil || attempt == 2 || !shortRetryableGitHubObservation(err) || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// A short retry cannot honor a GitHub rate-limit delay. A future durable
// corroboration worker must use delayed jobs for these observations.
func shortRetryableGitHubObservation(err error) bool {
	if !classifyGitHubCorroborationFailure(err).retryable {
		return false
	}
	var rateLimit *githubapi.RateLimitError
	return !errors.As(err, &rateLimit) ||
		(rateLimit.RetryAfter <= 0 && !rateLimit.ResetAt.After(time.Now()))
}

// Log only reconciler-authored classifications and allowlisted GitHub response
// metadata. Dependency errors can contain credentials or response bodies.
func (reconciler *OutcomeReconciler) logGitHubObservationFailure(request OutcomeReconciliation, operation string, pullRequestNumber int64, err error) {
	if reconciler.logger == nil {
		return
	}
	failure := classifyGitHubCorroborationFailure(err)
	attributes := []any{
		"workflow_id", request.Execution.WorkflowID,
		"agent_turn_id", request.Lease.ID,
		"execution_epoch", request.Lease.ExecutionEpoch,
		"role", request.Execution.Assignment.Role,
		"operation", operation,
		"pull_request_number", pullRequestNumber,
		"failure_code", failure.code,
	}
	if failure.status != 0 {
		attributes = append(attributes, "github_http_status", failure.status)
	}
	reconciler.logger.Warn("Agent Turn GitHub outcome observation failed", attributes...)
}

type openPullRequestEvidence struct {
	MutationID        string
	Title             string
	Body              string
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
		MutationID: mutation.ID, Title: arguments.Title, Body: arguments.Body,
		PullRequestID: result.PullRequestID, PullRequestNumber: result.Number, NodeID: result.NodeID,
		HeadSHA: result.HeadSHA, HeadRef: head, BaseRef: base,
	}, true
}

func blockedObservation(observedAt time.Time, promptOutcome json.RawMessage, mutation store.MutationReservation, workflowID string) store.AgentTurnSettlementObservation {
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
		return infrastructureObservation(observedAt, store.AgentTurnFailed, promptOutcome, "report_blocked terminal evidence is malformed or incoherent")
	}
	diagnostic := strings.TrimSpace(result.Reason)
	if details := strings.TrimSpace(result.Details); details != "" {
		diagnostic += ": " + details
	}
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
		execution.Turn.ControlRevision == lease.ControlRevision && role.ValidID(execution.Assignment.Role)
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
	return request.PromptError == PromptErrorDeadline || request.PromptError == PromptErrorCancellation ||
		request.PromptError == PromptErrorFailure || request.PromptError == PromptErrorInvalidResponse
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
		case mcp.ToolReportBlocked, mcp.ToolRequestReview, mcp.ToolSubmitReview, mcp.ToolConfirmPriorTerminalIntent:
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
		case PromptErrorInvalidResponse:
			diagnostic = "ACP prompt returned an invalid stop reason"
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

func infrastructureObservation(observedAt time.Time, status store.AgentTurnStatus, promptOutcome json.RawMessage, diagnostic string) store.AgentTurnSettlementObservation {
	return store.AgentTurnSettlementObservation{
		ObservedAt: observedAt, Outcome: workflow.TurnOutcomeInfrastructureFailed, Diagnostic: diagnostic,
		Completion: store.AgentTurnCompletion{Status: status, Outcome: promptOutcome, LastError: diagnostic},
	}
}

func (reconciler *OutcomeReconciler) sanitizeObservation(observation store.AgentTurnSettlementObservation, credential string) store.AgentTurnSettlementObservation {
	observation.Diagnostic = reconciler.sanitizeDiagnostic(observation.Diagnostic, credential)
	observation.Completion.LastError = reconciler.sanitizeDiagnostic(observation.Completion.LastError, credential)
	if observation.Outcome == workflow.TurnOutcomeBlocked && observation.Diagnostic == "" {
		observation.Outcome = workflow.TurnOutcomeInfrastructureFailed
		observation.Completion.Status = store.AgentTurnFailed
		observation.Diagnostic = "report_blocked terminal evidence has no safe diagnostic"
		observation.Completion.LastError = observation.Diagnostic
	}
	if observation.Outcome == workflow.TurnOutcomeInfrastructureFailed && observation.Diagnostic == "" {
		observation.Diagnostic = "Agent Turn outcome reconciliation failed"
		observation.Completion.LastError = observation.Diagnostic
	}
	return observation
}

func (reconciler *OutcomeReconciler) sanitizeDiagnostic(value, credential string) string {
	value = reconciler.redactProviderDiagnostic(value)
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
