package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	maximumVisibleEffectWorkerDuration = 365 * 24 * time.Hour
	maximumHandoffDiagnosticRunes      = 1000
)

var (
	ErrInvalidVisibleEffectWorkerConfiguration = errors.New("invalid GitHub visible-effect Worker configuration")
	ErrStalePullRequestHead                    = errors.New("Pull Request head is stale for durable readiness")
	ErrDuplicateHandoffMarker                  = errors.New("multiple GitHub comments contain the Human Handoff marker")
	ErrHandoffCommentReconciliation            = errors.New("GitHub Human Handoff comment did not converge")
)

// VisibleEffectWorkerStore is the durable boundary used by GitHub visible-effect workers.
type VisibleEffectWorkerStore interface {
	ClaimWorkflowGitHubEffectJob(context.Context, string, string, time.Duration) (*store.JobLease, error)
	HeartbeatJob(context.Context, store.JobLease, time.Duration) error
	GetWorkflowGitHubEffectContext(context.Context, store.JobLease) (store.WorkflowGitHubEffectContext, error)
	AcknowledgeWorkflowGitHubEffect(context.Context, store.JobLease, store.WorkflowGitHubEffectContext, json.RawMessage) (store.WorkflowGitHubEffectAcknowledgement, error)
	AcknowledgeWorkflowGitHubEffectFailure(context.Context, store.JobLease, store.WorkflowGitHubEffectContext, error, bool, time.Duration) (store.WorkflowGitHubEffectAcknowledgement, error)
	AcknowledgeWorkflowGitHubEffectCleanup(context.Context, store.JobLease) (store.WorkflowGitHubEffectAcknowledgement, error)
}

// VisibleEffectCredentialProvider supplies the Developer/Orchestrator repository credential.
type VisibleEffectCredentialProvider interface {
	RepositoryCredential(context.Context, string, string) (string, error)
}

type LabelWorkerAPI interface {
	LabelAPI
	GetPullRequest(context.Context, string, string, string, int) (PullRequest, error)
}

type HandoffWorkerAPI interface {
	ListIssueComments(context.Context, string, string, string, int) ([]IssueComment, error)
	CreateIssueComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	CreatePullRequestComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	DeleteIssueComment(context.Context, string, string, string, int64) error
}

// VisibleEffectWorkerConfig controls exact-kind claims, heartbeats, retries, and idle polling.
type VisibleEffectWorkerConfig struct {
	ClaimOwner        string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdlePollInterval  time.Duration
	RetryDelay        time.Duration
	OnError           func(error)
}

type visibleEffectWorker struct {
	store             VisibleEffectWorkerStore
	credentials       VisibleEffectCredentialProvider
	claimOwner        string
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idlePollInterval  time.Duration
	retryDelay        time.Duration
	onError           func(error)
}

// LabelWorker reconciles Workflow labels on the Work Item and active Change Proposal.
type LabelWorker struct {
	worker     visibleEffectWorker
	api        LabelWorkerAPI
	reconciler *LabelReconciler
}

// HumanHandoffWorker publishes idempotent Human Handoff diagnostics.
type HumanHandoffWorker struct {
	worker visibleEffectWorker
	api    HandoffWorkerAPI
}

var (
	_ VisibleEffectWorkerStore        = (*store.Store)(nil)
	_ VisibleEffectCredentialProvider = (*RepositoryInstallationCredentialProvider)(nil)
)

func NewLabelWorker(workerStore VisibleEffectWorkerStore, credentials VisibleEffectCredentialProvider, api LabelWorkerAPI, config VisibleEffectWorkerConfig) (*LabelWorker, error) {
	worker, err := newVisibleEffectWorker(workerStore, credentials, config)
	if err != nil || nilVisibleEffectDependency(api) {
		return nil, ErrInvalidVisibleEffectWorkerConfiguration
	}
	return &LabelWorker{worker: worker, api: api, reconciler: NewLabelReconciler(api)}, nil
}

func NewHumanHandoffWorker(workerStore VisibleEffectWorkerStore, credentials VisibleEffectCredentialProvider, api HandoffWorkerAPI, config VisibleEffectWorkerConfig) (*HumanHandoffWorker, error) {
	worker, err := newVisibleEffectWorker(workerStore, credentials, config)
	if err != nil || nilVisibleEffectDependency(api) {
		return nil, ErrInvalidVisibleEffectWorkerConfiguration
	}
	return &HumanHandoffWorker{worker: worker, api: api}, nil
}

func newVisibleEffectWorker(workerStore VisibleEffectWorkerStore, credentials VisibleEffectCredentialProvider, config VisibleEffectWorkerConfig) (visibleEffectWorker, error) {
	if nilVisibleEffectDependency(workerStore) || nilVisibleEffectDependency(credentials) || strings.TrimSpace(config.ClaimOwner) == "" ||
		!validVisibleEffectDuration(config.LeaseDuration) || !validVisibleEffectDuration(config.HeartbeatInterval) || config.HeartbeatInterval >= config.LeaseDuration ||
		!validVisibleEffectDuration(config.IdlePollInterval) || !validVisibleEffectDuration(config.RetryDelay) {
		return visibleEffectWorker{}, ErrInvalidVisibleEffectWorkerConfiguration
	}
	return visibleEffectWorker{
		store: workerStore, credentials: credentials, claimOwner: config.ClaimOwner,
		leaseDuration: config.LeaseDuration, heartbeatInterval: config.HeartbeatInterval,
		idlePollInterval: config.IdlePollInterval, retryDelay: config.RetryDelay, onError: config.OnError,
	}, nil
}

// ProcessNext claims and handles at most one RECONCILE_GITHUB_LABELS job.
func (worker *LabelWorker) ProcessNext(ctx context.Context) (bool, error) {
	return worker.worker.processNext(ctx, store.ReconcileGitHubLabelsJobKind, worker.reconcile, nil)
}

// Run processes label reconciliation jobs until its context ends.
func (worker *LabelWorker) Run(ctx context.Context) error {
	return worker.worker.run(ctx, worker.ProcessNext)
}

// ProcessNext claims and handles at most one PUBLISH_HUMAN_HANDOFF job.
func (worker *HumanHandoffWorker) ProcessNext(ctx context.Context) (bool, error) {
	return worker.worker.processNext(ctx, store.PublishHumanHandoffJobKind, worker.publish, worker.removeSupersededComments)
}

// Run processes Human Handoff publication jobs until its context ends.
func (worker *HumanHandoffWorker) Run(ctx context.Context) error {
	return worker.worker.run(ctx, worker.ProcessNext)
}

type visibleEffectOperation func(context.Context, string, store.WorkflowGitHubEffectContext, store.JobLease) (json.RawMessage, error)
type supersededVisibleEffectCleanup func(context.Context, string, store.WorkflowGitHubEffectContext, store.JobLease) error

func (worker *visibleEffectWorker) processNext(ctx context.Context, kind string, operation visibleEffectOperation, cleanup supersededVisibleEffectCleanup) (processed bool, resultErr error) {
	lease, err := worker.store.ClaimWorkflowGitHubEffectJob(ctx, kind, worker.claimOwner, worker.leaseDuration)
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", kind, err)
	}
	if lease == nil {
		return false, nil
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := worker.heartbeat(heartbeatCtx, *lease)
		if heartbeatErr != nil {
			cancelWork()
		}
		heartbeatDone <- heartbeatErr
	}()

	var credential string
	acknowledged := false
	operationErr := func() error {
		effect, err := worker.store.GetWorkflowGitHubEffectContext(workCtx, *lease)
		if err != nil {
			return fmt.Errorf("read latest durable Workflow GitHub effect context: %w", err)
		}
		if kind == store.PublishHumanHandoffJobKind && !effect.CleanupRequired &&
			!effect.SafetyDiagnostic && (effect.JobRevision != effect.Revision || effect.State != workflow.StateNeedsHuman) {
			acknowledgement, err := worker.store.AcknowledgeWorkflowGitHubEffect(workCtx, *lease, effect, json.RawMessage(`{"superseded":true}`))
			if err != nil {
				return fmt.Errorf("record superseded %s cleanup: %w", kind, err)
			}
			if acknowledgement.CleanupRequired {
				effect, err = worker.store.GetWorkflowGitHubEffectContext(workCtx, *lease)
				if err != nil {
					return fmt.Errorf("read superseded %s cleanup context: %w", kind, err)
				}
			}
		}
		credential, err = worker.credentials.RepositoryCredential(workCtx, effect.RepositoryOwner, effect.RepositoryName)
		if err != nil {
			if workCtx.Err() != nil {
				return err
			}
			_, failureErr := worker.failEffect(workCtx, *lease, effect, fmt.Errorf("obtain Developer/Orchestrator repository credential: %w", err))
			return failureErr
		}
		if strings.TrimSpace(credential) == "" {
			_, failureErr := worker.failEffect(workCtx, *lease, effect, permanentVisibleEffectError{cause: errors.New("Developer/Orchestrator repository credential is empty")})
			return failureErr
		}
		if effect.CleanupRequired {
			if cleanup == nil {
				return errors.New("superseded visible effect has no cleanup operation")
			}
			if err := cleanup(workCtx, credential, effect, *lease); err != nil {
				_, failureErr := worker.failEffect(workCtx, *lease, effect, redactVisibleEffectError(err, credential))
				return failureErr
			}
			if _, err := worker.store.AcknowledgeWorkflowGitHubEffectCleanup(workCtx, *lease); err != nil {
				return fmt.Errorf("acknowledge superseded %s cleanup: %w", kind, err)
			}
			acknowledged = true
			return nil
		}
		result, effectErr := operation(workCtx, credential, effect, *lease)
		if effectErr != nil {
			if workCtx.Err() != nil {
				return effectErr
			}
			acknowledgement, failureErr := worker.failEffect(workCtx, *lease, effect, redactVisibleEffectError(effectErr, credential))
			if !acknowledgement.CleanupRequired || cleanup == nil {
				return failureErr
			}
			if err := cleanup(workCtx, credential, effect, *lease); err != nil {
				_, cleanupFailureErr := worker.failEffect(workCtx, *lease, effect, redactVisibleEffectError(err, credential))
				return cleanupFailureErr
			}
			_, err = worker.store.AcknowledgeWorkflowGitHubEffectCleanup(workCtx, *lease)
			if err == nil {
				acknowledged = true
			}
			return err
		}
		acknowledgement, err := worker.store.AcknowledgeWorkflowGitHubEffect(workCtx, *lease, effect, result)
		if err != nil {
			return fmt.Errorf("acknowledge %s: %w", kind, err)
		}
		if acknowledgement.Superseded && cleanup != nil {
			if err := cleanup(workCtx, credential, effect, *lease); err != nil {
				_, cleanupFailureErr := worker.failEffect(workCtx, *lease, effect, redactVisibleEffectError(err, credential))
				return cleanupFailureErr
			}
			if _, err := worker.store.AcknowledgeWorkflowGitHubEffectCleanup(workCtx, *lease); err != nil {
				return fmt.Errorf("acknowledge superseded %s cleanup: %w", kind, err)
			}
		}
		acknowledged = true
		return nil
	}()

	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork()
	operationErr = redactVisibleEffectError(operationErr, credential)
	if operationErr == nil {
		return true, nil
	}
	if heartbeatErr != nil && !(acknowledged && errors.Is(heartbeatErr, store.ErrJobLeaseLost)) {
		return true, errors.Join(fmt.Errorf("heartbeat %s: %w", kind, heartbeatErr), operationErr)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, operationErr
}

func (worker *visibleEffectWorker) failEffect(ctx context.Context, lease store.JobLease, effect store.WorkflowGitHubEffectContext, cause error) (store.WorkflowGitHubEffectAcknowledgement, error) {
	retryable, delay := worker.classify(cause)
	acknowledgement, err := worker.store.AcknowledgeWorkflowGitHubEffectFailure(ctx, lease, effect, cause, retryable, delay)
	if err != nil {
		return acknowledgement, errors.Join(cause, fmt.Errorf("record GitHub visible-effect failure: %w", err))
	}
	if acknowledgement.Superseded {
		if !acknowledgement.CleanupRequired {
			return acknowledgement, nil
		}
		return acknowledgement, cause
	}
	return acknowledgement, cause
}

func (worker *visibleEffectWorker) heartbeat(ctx context.Context, lease store.JobLease) error {
	ticker := time.NewTicker(worker.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := worker.store.HeartbeatJob(ctx, lease, worker.leaseDuration); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func (worker *visibleEffectWorker) classify(err error) (bool, time.Duration) {
	metadata := ExtractSafeErrorMetadata(err)
	if metadata.Permanent {
		return false, worker.retryDelay
	}
	if metadata.Transient || metadata.APIRetryable {
		return true, worker.retryAfter(metadata)
	}
	if metadata.APIClientError {
		return false, worker.retryDelay
	}
	return true, worker.retryAfter(metadata)
}

func (worker *visibleEffectWorker) retryAfter(metadata SafeErrorMetadata) time.Duration {
	delay := worker.retryDelay
	rateLimitDelay := metadata.RetryAfter
	if rateLimitDelay <= 0 && !metadata.ResetAt.IsZero() {
		rateLimitDelay = time.Until(metadata.ResetAt)
	}
	if rateLimitDelay > delay {
		delay = min(rateLimitDelay, maximumVisibleEffectWorkerDuration)
	}
	return delay
}

func (worker *visibleEffectWorker) run(ctx context.Context, process func(context.Context) (bool, error)) error {
	for {
		processed, err := process(ctx)
		if err != nil && ctx.Err() == nil && worker.onError != nil {
			worker.onError(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && processed {
			continue
		}
		if err := waitVisibleEffectWorker(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func (worker *LabelWorker) reconcile(ctx context.Context, credential string, effect store.WorkflowGitHubEffectContext, _ store.JobLease) (json.RawMessage, error) {
	desired, err := managedLabelState(effect.State)
	if err != nil {
		return nil, err
	}
	issueNumber, err := positiveGitHubNumber(effect.IssueNumber)
	if err != nil {
		return nil, err
	}
	targets := []int{issueNumber}
	if effect.ChangeProposal != nil {
		pullRequestNumber, err := positiveGitHubNumber(effect.ChangeProposal.Number)
		if err != nil {
			return nil, err
		}
		targets = append(targets, pullRequestNumber)
	}

	if desired == StatePRReady {
		for _, target := range targets {
			if err := worker.reconciler.ReconcileState(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, target, StateNone); err != nil {
				return nil, err
			}
		}
		if effect.ChangeProposal == nil {
			return nil, &ConfigurationError{Cause: errors.New("PR_READY Workflow has no active Change Proposal")}
		}
		pullRequest, getErr := worker.api.GetPullRequest(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, targets[1])
		if getErr != nil {
			return nil, getErr
		}
		if effect.ReadyForSHA == "" || pullRequest.Head.SHA != effect.ReadyForSHA || effect.ChangeProposal.HeadSHA != effect.ReadyForSHA {
			return nil, stalePullRequestHeadError{remote: pullRequest.Head.SHA, ready: effect.ReadyForSHA, durable: effect.ChangeProposal.HeadSHA}
		}
	}

	for _, target := range targets {
		if err := worker.reconciler.ReconcileState(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, target, desired); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]any{"state": effect.State, "revision": effect.Revision, "targets": len(targets)})
}

func managedLabelState(state workflow.State) (WorkflowState, error) {
	switch state {
	case workflow.StateDeveloping:
		return StateDeveloping, nil
	case workflow.StateReviewing:
		return StateReviewing, nil
	case workflow.StatePRReady:
		return StatePRReady, nil
	case workflow.StateNeedsHuman:
		return StateNeedsHuman, nil
	case workflow.StateDormant, workflow.StateClosing, workflow.StateClosed:
		return StateNone, nil
	default:
		return StateNone, &ConfigurationError{Cause: fmt.Errorf("%w: Workflow state %q", ErrInvalidWorkflowState, state)}
	}
}

func (worker *HumanHandoffWorker) publish(ctx context.Context, credential string, effect store.WorkflowGitHubEffectContext, lease store.JobLease) (json.RawMessage, error) {
	if !effect.SafetyDiagnostic && (effect.JobRevision != effect.Revision || effect.State != workflow.StateNeedsHuman) {
		return json.Marshal(map[string]any{"superseded": true, "current_revision": effect.Revision, "current_state": effect.State})
	}
	if strings.TrimSpace(effect.HandoffReason) == "" {
		return nil, &ConfigurationError{Cause: errors.New("Human Handoff has no reason")}
	}
	marker, err := RenderMarker(Marker{WorkflowID: effect.WorkflowID, OperationID: lease.ID})
	if err != nil {
		return nil, err
	}
	body := handoffCommentBody(effect.HandoffReason, effect.HandoffDiagnostic, credential)
	issueNumber, err := positiveGitHubNumber(effect.IssueNumber)
	if err != nil {
		return nil, err
	}
	markerIdentity := Marker{WorkflowID: effect.WorkflowID, OperationID: lease.ID}
	issueComment, err := worker.ensureComment(ctx, credential, effect, issueNumber, markerIdentity, marker, body, false)
	if err != nil {
		return nil, err
	}
	result := handoffPublicationResult{Issue: commentReference{ID: issueComment.ID, URL: issueComment.HTMLURL}}
	if effect.ChangeProposal != nil {
		pullRequestNumber, err := positiveGitHubNumber(effect.ChangeProposal.Number)
		if err != nil {
			return nil, err
		}
		pullRequestComment, err := worker.ensureComment(ctx, credential, effect, pullRequestNumber, markerIdentity, marker, body, true)
		if err != nil {
			return nil, err
		}
		result.PullRequest = &commentReference{ID: pullRequestComment.ID, URL: pullRequestComment.HTMLURL}
	}
	return json.Marshal(result)
}

type handoffPublicationResult struct {
	Issue       commentReference  `json:"issue"`
	PullRequest *commentReference `json:"pull_request,omitempty"`
}

type commentReference struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

func (worker *HumanHandoffWorker) ensureComment(ctx context.Context, credential string, effect store.WorkflowGitHubEffectContext, number int, markerIdentity Marker, marker, body string, pullRequest bool) (IssueComment, error) {
	comments, err := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
	if err != nil {
		return IssueComment{}, err
	}
	if existing, found, err := worker.reconcileMarkedComments(ctx, credential, effect, number, comments, markerIdentity); found || err != nil {
		return existing, err
	}
	request := CommentRequest{Body: body, Marker: marker}
	if pullRequest {
		_, err = worker.api.CreatePullRequestComment(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number, request)
	} else {
		_, err = worker.api.CreateIssueComment(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number, request)
	}
	comments, observeErr := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
	if observeErr != nil {
		return IssueComment{}, errors.Join(err, observeErr)
	}
	if existing, found, markerErr := worker.reconcileMarkedComments(ctx, credential, effect, number, comments, markerIdentity); found || markerErr != nil {
		return existing, markerErr
	}
	if err == nil {
		return IssueComment{}, handoffCommentConvergenceError{cause: errors.New("created comment was not observable")}
	}
	return IssueComment{}, err
}

func (worker *HumanHandoffWorker) reconcileMarkedComments(ctx context.Context, credential string, effect store.WorkflowGitHubEffectContext, number int, comments []IssueComment, marker Marker) (IssueComment, bool, error) {
	marked := exactMarkedComments(comments, marker)
	if len(marked) == 0 {
		return IssueComment{}, false, nil
	}
	sort.Slice(marked, func(left, right int) bool { return marked[left].ID < marked[right].ID })
	retained := marked[0]
	if len(marked) == 1 {
		return retained, true, nil
	}
	for _, extra := range marked[1:] {
		deleteErr := worker.api.DeleteIssueComment(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, extra.ID)
		if deleteErr == nil || isAPIStatus(deleteErr, 404) {
			continue
		}
		observed, observeErr := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
		if observeErr != nil {
			return IssueComment{}, false, errors.Join(deleteErr, observeErr)
		}
		if markedCommentWithID(observed, marker, extra.ID) {
			return IssueComment{}, false, deleteErr
		}
	}
	observed, err := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
	if err != nil {
		return IssueComment{}, false, err
	}
	marked = exactMarkedComments(observed, marker)
	if len(marked) > 1 {
		return IssueComment{}, false, duplicateHandoffMarkerError{count: len(marked)}
	}
	if len(marked) != 1 || marked[0].ID != retained.ID {
		return IssueComment{}, false, handoffCommentConvergenceError{cause: errors.New("deterministically retained comment was not observable")}
	}
	return marked[0], true, nil
}

func exactMarkedComments(comments []IssueComment, marker Marker) []IssueComment {
	marked := make([]IssueComment, 0, len(comments))
	for _, comment := range comments {
		for _, parsed := range ParseMarkers(comment.Body) {
			if parsed == marker {
				marked = append(marked, comment)
				break
			}
		}
	}
	return marked
}

func markedCommentWithID(comments []IssueComment, marker Marker, commentID int64) bool {
	for _, comment := range exactMarkedComments(comments, marker) {
		if comment.ID == commentID {
			return true
		}
	}
	return false
}

func (worker *HumanHandoffWorker) removeSupersededComments(ctx context.Context, credential string, effect store.WorkflowGitHubEffectContext, lease store.JobLease) error {
	marker := Marker{WorkflowID: effect.WorkflowID, OperationID: lease.ID}
	issueNumber := effect.CleanupIssueNumber
	if issueNumber == 0 {
		issueNumber = effect.IssueNumber
	}
	targets := []int64{issueNumber}
	pullRequestNumber := effect.CleanupPullRequestNumber
	if pullRequestNumber == 0 && effect.ChangeProposal != nil {
		pullRequestNumber = effect.ChangeProposal.Number
	}
	if pullRequestNumber > 0 && pullRequestNumber != issueNumber {
		targets = append(targets, pullRequestNumber)
	}
	var cleanupErr error
	for _, target := range targets {
		number, err := positiveGitHubNumber(target)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		comments, listErr := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
		if listErr != nil {
			cleanupErr = errors.Join(cleanupErr, listErr)
			continue
		}
		for _, comment := range exactMarkedComments(comments, marker) {
			deleteErr := worker.api.DeleteIssueComment(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, comment.ID)
			if deleteErr != nil && !isAPIStatus(deleteErr, 404) {
				observed, observeErr := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
				if observeErr != nil || markedCommentWithID(observed, marker, comment.ID) {
					cleanupErr = errors.Join(cleanupErr, deleteErr, observeErr)
				}
			}
		}
		observed, observeErr := worker.api.ListIssueComments(ctx, credential, effect.RepositoryOwner, effect.RepositoryName, number)
		if observeErr != nil {
			cleanupErr = errors.Join(cleanupErr, observeErr)
		} else if len(exactMarkedComments(observed, marker)) != 0 {
			cleanupErr = errors.Join(cleanupErr, ErrHandoffCommentReconciliation)
		}
	}
	return cleanupErr
}

func handoffCommentBody(reason, diagnostic, credential string) string {
	reason = sanitizeHandoffText(reason, "", 160)
	diagnostic = sanitizeHandoffText(diagnostic, credential, maximumHandoffDiagnosticRunes)
	body := "Human handoff required: " + reason + "."
	if diagnostic != "" {
		body += "\n\n" + diagnostic
	}
	return body
}

func sanitizeHandoffText(value, credential string, maximumRunes int) string {
	if credential != "" {
		value = strings.ReplaceAll(value, credential, "[REDACTED]")
	}
	value = strings.ReplaceAll(value, "<!--", "&lt;!--")
	value = strings.ReplaceAll(value, "-->", "--&gt;")
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maximumRunes {
		value = string(runes[:maximumRunes])
	}
	return value
}

type stalePullRequestHeadError struct {
	remote  string
	ready   string
	durable string
}

func (err stalePullRequestHeadError) Error() string {
	return fmt.Sprintf("%v: remote %q, ready %q, durable %q", ErrStalePullRequestHead, err.remote, err.ready, err.durable)
}
func (err stalePullRequestHeadError) Unwrap() error   { return ErrStalePullRequestHead }
func (err stalePullRequestHeadError) Transient() bool { return true }

type duplicateHandoffMarkerError struct{ count int }

func (err duplicateHandoffMarkerError) Error() string {
	return fmt.Sprintf("%v: found %d", ErrDuplicateHandoffMarker, err.count)
}
func (err duplicateHandoffMarkerError) Unwrap() error   { return ErrDuplicateHandoffMarker }
func (err duplicateHandoffMarkerError) Transient() bool { return true }

type handoffCommentConvergenceError struct{ cause error }

func (err handoffCommentConvergenceError) Error() string {
	return fmt.Sprintf("%v: %v", ErrHandoffCommentReconciliation, err.cause)
}
func (err handoffCommentConvergenceError) Unwrap() error   { return ErrHandoffCommentReconciliation }
func (err handoffCommentConvergenceError) Transient() bool { return true }

type permanentVisibleEffectError struct{ cause error }

func (err permanentVisibleEffectError) Error() string   { return err.cause.Error() }
func (err permanentVisibleEffectError) Unwrap() error   { return err.cause }
func (err permanentVisibleEffectError) Permanent() bool { return true }

type credentialSafeVisibleEffectError struct {
	message  string
	metadata SafeErrorMetadata
}

func (err credentialSafeVisibleEffectError) Error() string { return err.message }
func (err credentialSafeVisibleEffectError) SafeErrorMetadata() SafeErrorMetadata {
	return err.metadata
}

func redactVisibleEffectError(err error, credential string) error {
	if err == nil || credential == "" {
		return err
	}
	return credentialSafeVisibleEffectError{
		message:  strings.ReplaceAll(err.Error(), credential, "[REDACTED]"),
		metadata: ExtractSafeErrorMetadata(err),
	}
}

func positiveGitHubNumber(number int64) (int, error) {
	converted := int(number)
	if number <= 0 || int64(converted) != number {
		return 0, &ConfigurationError{Cause: errors.New("GitHub resource number is out of range")}
	}
	return converted, nil
}

func validVisibleEffectDuration(duration time.Duration) bool {
	return duration >= time.Microsecond && duration <= maximumVisibleEffectWorkerDuration
}

func nilVisibleEffectDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func waitVisibleEffectWorker(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
