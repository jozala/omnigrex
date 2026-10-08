package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

type terminalCorroborationStore interface {
	ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetTerminalCorroborationContext(context.Context, store.JobLease) (store.TerminalCorroborationContext, error)
	RetryTerminalCorroboration(context.Context, store.JobLease, string, time.Duration) error
	SettleTerminalCorroboration(context.Context, store.JobLease, store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error)
	SettleTerminalRevalidation(context.Context, store.JobLease, int64, store.AgentTurnSettlementObservation) error
	CompleteTerminalCorroborationHandoff(context.Context, store.JobLease, workflow.Reason, string) (store.AgentTurnSettlement, error)
	CompleteTerminalRevalidationHandoff(context.Context, store.JobLease, workflow.Reason, string) error
	RedirectRevalidationToFreshTurn(ctx context.Context, lease store.JobLease, expectedRevision int64, headSHA string, pullRequestID, pullRequestNumber int64) error
}

var _ terminalCorroborationStore = (*store.Store)(nil)

type TerminalCorroborationWorkerConfig struct {
	ClaimOwner    string
	Window        time.Duration
	PollInterval  time.Duration
	LeaseDuration time.Duration
	OnError       func(error)
}

type TerminalCorroborationWorker struct {
	store        terminalCorroborationStore
	outcomes     *OutcomeReconciler
	developer    RepositoryCredentialProvider
	reviewer     RepositoryCredentialProvider
	pullRequests ChangeProposalVerifier
	workspace    interface {
		Paths(string) (workspace.Paths, error)
	}
	config TerminalCorroborationWorkerConfig
}

func NewTerminalCorroborationWorker(database terminalCorroborationStore, outcomes *OutcomeReconciler,
	developer, reviewer RepositoryCredentialProvider, paths interface {
		Paths(string) (workspace.Paths, error)
	},
	config TerminalCorroborationWorkerConfig) (*TerminalCorroborationWorker, error) {
	if database == nil || outcomes == nil || developer == nil || reviewer == nil || paths == nil ||
		config.ClaimOwner == "" || config.Window <= 0 || config.PollInterval <= 0 || config.LeaseDuration < 3*time.Second {
		return nil, errors.New("invalid terminal corroboration Worker configuration")
	}
	return &TerminalCorroborationWorker{store: database, outcomes: outcomes, developer: developer,
		reviewer: reviewer, workspace: paths, config: config}, nil
}

// WithChangeProposalVerifier adds authoritative GitHub verification for
// terminal-intent revalidation and returns the worker for chaining. With a
// verifier, a changed live head redirects the obsolete intent to a fresh Turn
// instead of adopting it; without one, revalidation keeps its existing
// behavior.
func (worker *TerminalCorroborationWorker) WithChangeProposalVerifier(verifier ChangeProposalVerifier) *TerminalCorroborationWorker {
	worker.pullRequests = verifier
	return worker
}

func (worker *TerminalCorroborationWorker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		processed, err := worker.ProcessOne(ctx)
		if err != nil && worker.config.OnError != nil {
			worker.config.OnError(errors.New("terminal corroboration Worker could not complete an attempt"))
		}
		if processed && err == nil {
			continue
		}
		timer := time.NewTimer(worker.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

// ProcessOne performs no agent prompt or GitHub mutation. Every durable result
// is fenced by the claimed verification job's exact attempt and lease token.
func (worker *TerminalCorroborationWorker) ProcessOne(ctx context.Context) (processed bool, err error) {
	lease, err := worker.store.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind,
		worker.config.ClaimOwner, worker.config.LeaseDuration)
	if err == nil && lease == nil {
		lease, err = worker.store.ClaimJobKind(ctx, "agent-turn-recovery", store.RevalidateTerminalIntentJobKind,
			worker.config.ClaimOwner, worker.config.LeaseDuration)
	}
	if err != nil || lease == nil {
		return false, err
	}
	ctx, operation := telemetry.StartOperation(ctx, telemetry.AgentTurnReconcileOutcome, jobAttributes(*lease)...)
	defer finishOperation(ctx, operation, &err)
	workCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(worker.config.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := worker.store.HeartbeatJob(workCtx, *lease, worker.config.LeaseDuration); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-finished }()
	ctx = workCtx
	pending, err := worker.store.GetTerminalCorroborationContext(ctx, *lease)
	if errors.Is(err, store.ErrJobLeaseLost) {
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
		return true, nil // Closure or another fenced transition won.
	}
	if errors.Is(err, store.ErrAgentTurnSettlementRejected) && lease.Kind == store.RevalidateTerminalIntentJobKind {
		return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "invalid_configuration")
	}
	if err != nil {
		return true, err
	}
	remaining := pending.Checkpoint.PendingSince.Add(worker.config.Window).Sub(time.Now())
	if remaining <= 0 {
		return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationExhausted, pending.Checkpoint.LastFailureCode)
	}
	var credential string
	role := pending.Execution.Assignment.Role
	switch role {
	case workflow.RoleDeveloper:
		credential, err = worker.developer.RepositoryCredential(ctx, pending.Execution.Repository.Owner, pending.Execution.Repository.Name)
	case workflow.RoleReviewer:
		credential, err = worker.reviewer.RepositoryCredential(ctx, pending.Execution.Repository.Owner, pending.Execution.Repository.Name)
	default:
		return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "invalid_configuration")
	}
	if err != nil {
		classification := classifyGitHubCorroborationFailure(err)
		metadata := githubapi.ExtractSafeErrorMetadata(err)
		retryAfter := metadata.RetryAfter
		if untilReset := time.Until(metadata.ResetAt); untilReset > retryAfter {
			retryAfter = untilReset
		}
		return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: classification.code, Retryable: classification.retryable,
				Prerequisite: classification.prerequisite, RetryAfter: retryAfter})
	}
	if credential == "" {
		return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "invalid_configuration")
	}
	if lease.Kind == store.RevalidateTerminalIntentJobKind && worker.pullRequests != nil && pending.Execution.ChangeProposal != nil {
		redirected, redirectErr := worker.redirectChangedHeadRevalidation(ctx, *lease, pending, credential, remaining)
		if redirectErr != nil {
			return true, redirectErr
		}
		if redirected {
			return true, nil
		}
	}
	paths, err := worker.workspace.Paths(pending.Execution.Assignment.ID)
	if err != nil {
		return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: "workspace_observation_unavailable", Retryable: true})
	}
	turn := pending.Execution.Turn
	request := OutcomeReconciliation{
		Lease:     store.AgentTurnLease{AgentTurn: turn, JobLease: store.JobLease{Job: store.Job{WorkflowID: pending.Checkpoint.WorkflowID}}},
		Execution: pending.Execution, Paths: paths,
		PriorPublicationMutations: pending.PriorPublicationMutations,
	}
	if role == workflow.RoleReviewer {
		request.ReviewerRepositoryCredential = credential
	} else {
		request.RepositoryCredential = credential
	}
	if pending.Checkpoint.PromptError != "" {
		request.PromptError = PromptErrorClassification(pending.Checkpoint.PromptError)
		request.PromptDiagnostic = "the ACP response was unavailable before corroboration"
	} else {
		var outcome struct {
			StopReason acp.StopReason `json:"stop_reason"`
		}
		if json.Unmarshal(pending.Checkpoint.PromptOutcome, &outcome) != nil || outcome.StopReason != acp.StopReasonEndTurn {
			return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "invalid_configuration")
		}
		request.PromptResponse = &acp.PromptResponse{StopReason: outcome.StopReason}
	}
	var failure *TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed TerminalCorroborationFailure) { failure = &observed }
	observation, err := worker.outcomes.ReconcileRecorded(ctx, request, pending.Mutations)
	if err != nil {
		return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: "database_observation_unavailable", Retryable: true})
	}
	if failure != nil {
		if failure.SourceInvocationID != pending.Checkpoint.SourceInvocationID {
			return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
		}
		return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining, *failure)
	}
	switch observation.Outcome {
	case workflow.TurnOutcomeChangeProposalReady, workflow.TurnOutcomeChangesRequested, workflow.TurnOutcomeApproved:
		if lease.Kind == store.RevalidateTerminalIntentJobKind {
			err = worker.store.SettleTerminalRevalidation(ctx, *lease, pending.WorkflowRevision, observation)
		} else {
			_, err = worker.store.SettleTerminalCorroboration(ctx, *lease, observation)
		}
		if err == nil || errors.Is(err, store.ErrJobLeaseLost) {
			if err != nil {
				telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
			}
			return true, nil
		}
		if errors.Is(err, store.ErrTerminalCorroborationHeadMoved) {
			return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining,
				TerminalCorroborationFailure{Code: "head_changed_during_corroboration", Retryable: true})
		}
		if errors.Is(err, store.ErrTerminalCorroborationConflict) || errors.Is(err, store.ErrAgentTurnSettlementRejected) ||
			errors.Is(err, store.ErrAgentTurnChangeProposalConflict) || errors.Is(err, store.ErrReviewerActorConflict) ||
			errors.Is(err, store.ErrAgentTurnSettlementInvalid) {
			return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
		}
		return true, worker.retryOrHandoff(ctx, *lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: "database_observation_unavailable", Retryable: true})
	default:
		return true, worker.handoff(ctx, *lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
	}
}

// redirectChangedHeadRevalidation reports whether revalidation is complete for
// this iteration: true after a redirect, an acknowledged retry, or an
// acknowledged handoff, and false only when an explicitly successful
// matching-head verification lets ordinary revalidation continue. It fetches
// the current GitHub Pull Request outside database locks; when its head moved
// beyond the source Turn's expected head, the obsolete intent is never
// adopted. Acknowledged outcomes release job authority, so the iteration must
// stop instead of falling through into workspace inspection and outcome
// reconciliation.
func (worker *TerminalCorroborationWorker) redirectChangedHeadRevalidation(ctx context.Context, lease store.JobLease, pending store.TerminalCorroborationContext, credential string, remaining time.Duration) (bool, error) {
	durable := pending.Execution.ChangeProposal
	live, err := worker.pullRequests.GetPullRequest(ctx, credential, pending.Execution.Repository.Owner, pending.Execution.Repository.Name, int(durable.PullRequestNumber))
	if err != nil {
		classification := classifyGitHubCorroborationFailure(err)
		metadata := githubapi.ExtractSafeErrorMetadata(err)
		retryAfter := metadata.RetryAfter
		if untilReset := time.Until(metadata.ResetAt); untilReset > retryAfter {
			retryAfter = untilReset
		}
		return true, worker.retryOrHandoff(ctx, lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: classification.code, Retryable: classification.retryable,
				Prerequisite: classification.prerequisite, RetryAfter: retryAfter})
	}
	head, err := VerifyObservedChangeProposal(durable.PullRequestID, durable.PullRequestNumber, durable.HeadRef, durable.BaseRef, live)
	if err != nil {
		return true, worker.handoff(ctx, lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
	}
	if head == pending.Execution.Turn.ExpectedHeadSHA {
		return false, nil
	}
	if err := worker.store.RedirectRevalidationToFreshTurn(ctx, lease, pending.WorkflowRevision, head, live.ID, int64(live.Number)); err != nil {
		if errors.Is(err, store.ErrJobLeaseLost) {
			telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
			return true, nil
		}
		if errors.Is(err, store.ErrTerminalCorroborationHeadMoved) {
			return true, worker.retryOrHandoff(ctx, lease, pending.Checkpoint, remaining,
				TerminalCorroborationFailure{Code: "head_changed_during_corroboration", Retryable: true})
		}
		if errors.Is(err, store.ErrTerminalCorroborationConflict) || errors.Is(err, store.ErrAgentTurnSettlementRejected) ||
			errors.Is(err, store.ErrAgentTurnChangeProposalConflict) || errors.Is(err, store.ErrReviewerActorConflict) ||
			errors.Is(err, store.ErrAgentTurnSettlementInvalid) {
			return true, worker.handoff(ctx, lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
		}
		return true, worker.retryOrHandoff(ctx, lease, pending.Checkpoint, remaining,
			TerminalCorroborationFailure{Code: "database_observation_unavailable", Retryable: true})
	}
	return true, nil
}

func (worker *TerminalCorroborationWorker) handoff(ctx context.Context, lease store.JobLease, reason workflow.Reason, code string) error {
	telemetry.SetOutcome(ctx, telemetry.Failure, "corroboration_handoff")
	var err error
	if lease.Kind == store.RevalidateTerminalIntentJobKind {
		err = worker.store.CompleteTerminalRevalidationHandoff(ctx, lease, reason, code)
	} else {
		_, err = worker.store.CompleteTerminalCorroborationHandoff(ctx, lease, reason, code)
	}
	if errors.Is(err, store.ErrJobLeaseLost) {
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
		return nil
	}
	return err
}

func (worker *TerminalCorroborationWorker) retryOrHandoff(ctx context.Context, lease store.JobLease, checkpoint store.TerminalCorroboration,
	remaining time.Duration, failure TerminalCorroborationFailure) error {
	telemetry.SetOutcome(ctx, telemetry.Failure, "corroboration_unavailable")
	if failure.Prerequisite {
		return worker.handoff(ctx, lease, workflow.ReasonTerminalCorroborationPrerequisite, failure.Code)
	}
	if !failure.Retryable {
		return worker.handoff(ctx, lease, workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
	}
	delay := terminalCorroborationBackoff(lease.Attempt)
	if failure.RetryAfter > delay {
		delay = failure.RetryAfter
	}
	if delay > remaining {
		delay = remaining
	}
	if delay < 0 || checkpoint.PendingSince.IsZero() {
		return fmt.Errorf("invalid terminal corroboration retry state")
	}
	return worker.store.RetryTerminalCorroboration(ctx, lease, failure.Code, delay)
}

func terminalCorroborationBackoff(attempt int) time.Duration {
	delay := 5 * time.Second
	for index := 1; index < attempt && delay < 2*time.Minute; index++ {
		delay *= 2
	}
	if delay > 2*time.Minute {
		return 2 * time.Minute
	}
	return delay
}
