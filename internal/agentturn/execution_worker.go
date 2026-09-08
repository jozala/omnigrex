package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/gitremote"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

var ErrInvalidExecutionWorker = errors.New("invalid Agent Turn execution Worker")

const finalizationAttemptLimit = 3

// ExecutionWorkerStore is the durable execution, mutation-barrier, and settlement boundary.
type ExecutionWorkerStore interface {
	ClaimAndAcquireAgentTurn(context.Context, string, time.Duration, int) (store.AgentTurnLease, bool, error)
	RefreshAgentTurnLease(context.Context, store.AgentTurnLease, time.Duration) (store.AgentTurnLease, error)
	GetAgentTurnExecutionContext(context.Context, store.AgentTurnLease) (store.AgentTurnExecutionContext, error)
	OpenMutationAdmission(context.Context, store.AgentTurnLease) error
	CloseMutationAdmission(context.Context, store.AgentTurnLease) error
	ListUnsettledMutations(context.Context, store.AgentTurnLease) ([]store.MutationReservation, error)
	FailMutation(context.Context, store.AgentTurnLease, string, error) error
	MarkMutationUnknown(context.Context, store.AgentTurnLease, string, error) error
	BeginAgentTurnRecovery(context.Context, store.AgentTurnLease) (store.AgentTurnRecovery, error)
	SettleAgentTurn(context.Context, store.AgentTurnLease, store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error)
}

// DefaultBranchResolver observes the current default branch with the selected Role credential.
type DefaultBranchResolver interface {
	ResolveDefaultBranch(context.Context, string, string, string) (githubapi.DefaultBranch, error)
}

// ExecutionRuntime is the minimal live runtime retained after launch readiness.
type ExecutionRuntime interface {
	CurrentLease() store.AgentTurnLease
	PromptClient() session.PromptClient
	CloseMCP(context.Context) error
	Cleanup(context.Context) error
}

// ExecutionLauncher launches one prepared runtime without prompting it.
type ExecutionLauncher interface {
	LaunchExecution(context.Context, LaunchRequest) (ExecutionRuntime, error)
}

// ExecutionPrompter submits the sole structured Agent Turn prompt.
type ExecutionPrompter interface {
	Prompt(context.Context, session.PromptRequest) (acp.PromptResponse, error)
}

// ExecutionOutcomeReconciler derives a settlement observation from durable and fresh evidence.
type ExecutionOutcomeReconciler interface {
	Reconcile(context.Context, OutcomeReconciliation) (store.AgentTurnSettlementObservation, error)
}

// ExecutionWorkspace resolves deterministic assignment paths for outcome observation.
type ExecutionWorkspace interface {
	Paths(string) (workspace.Paths, error)
}

var (
	_ ExecutionWorkerStore       = (*store.Store)(nil)
	_ DefaultBranchResolver      = (*githubapi.APIClient)(nil)
	_ ExecutionLauncher          = (*Launcher)(nil)
	_ ExecutionPrompter          = (*session.Coordinator)(nil)
	_ ExecutionOutcomeReconciler = (*OutcomeReconciler)(nil)
	_ ExecutionWorkspace         = (*workspace.Lifecycle)(nil)
)

// ExecutionWorkerDependencies are stable production services, each narrowed to this worker's use.
type ExecutionWorkerDependencies struct {
	Store                ExecutionWorkerStore
	DeveloperCredentials RepositoryCredentialProvider
	ReviewerCredentials  RepositoryCredentialProvider
	DefaultBranch        DefaultBranchResolver
	Launcher             ExecutionLauncher
	Sessions             ExecutionPrompter
	Outcomes             ExecutionOutcomeReconciler
	Workspace            ExecutionWorkspace
}

// ExecutionWorkerConfig controls execution ownership, deadlines, capacity, and in-memory provider credentials.
type ExecutionWorkerConfig struct {
	ClaimOwner                      string
	LeaseDuration                   time.Duration
	HeartbeatInterval               time.Duration
	IdlePollInterval                time.Duration
	TurnTimeout                     time.Duration
	CleanupTimeout                  time.Duration
	ConcurrencyLimit                int
	DeveloperProviderCredentialJSON json.RawMessage
	ReviewerProviderCredentialJSON  json.RawMessage
	GitRemoteBaseURL                string
	OnError                         func(error)
}

func (ExecutionWorkerConfig) String() string { return "Agent Turn execution Worker config" }
func (ExecutionWorkerConfig) GoString() string {
	return "agentturn.ExecutionWorkerConfig{<credentials redacted>}"
}

// ExecutionWorker claims and executes RUN_AGENT_TURN actions without owning process construction details.
type ExecutionWorker struct {
	store                ExecutionWorkerStore
	developerCredentials RepositoryCredentialProvider
	reviewerCredentials  RepositoryCredentialProvider
	defaultBranch        DefaultBranchResolver
	launcher             ExecutionLauncher
	sessions             ExecutionPrompter
	outcomes             ExecutionOutcomeReconciler
	workspace            ExecutionWorkspace
	claimOwner           string
	leaseDuration        time.Duration
	heartbeatInterval    time.Duration
	idlePollInterval     time.Duration
	turnTimeout          time.Duration
	cleanupTimeout       time.Duration
	concurrencyLimit     int
	developerProvider    json.RawMessage
	reviewerProvider     json.RawMessage
	gitRemoteBase        gitremote.BaseURL
	onError              func(error)
}

func (*ExecutionWorker) String() string   { return "Agent Turn execution Worker" }
func (*ExecutionWorker) GoString() string { return "agentturn.ExecutionWorker{<credentials redacted>}" }

// NewExecutionWorker validates all dependencies and takes private copies of provider credentials.
func NewExecutionWorker(dependencies ExecutionWorkerDependencies, config ExecutionWorkerConfig) (*ExecutionWorker, error) {
	if nilDependency(dependencies.Store) || nilDependency(dependencies.DeveloperCredentials) ||
		nilDependency(dependencies.ReviewerCredentials) || nilDependency(dependencies.DefaultBranch) ||
		nilDependency(dependencies.Launcher) || nilDependency(dependencies.Sessions) ||
		nilDependency(dependencies.Outcomes) || nilDependency(dependencies.Workspace) {
		return nil, fmt.Errorf("%w: dependency is nil", ErrInvalidExecutionWorker)
	}
	if strings.TrimSpace(config.ClaimOwner) == "" || strings.TrimSpace(config.ClaimOwner) != config.ClaimOwner {
		return nil, fmt.Errorf("%w: claim owner", ErrInvalidExecutionWorker)
	}
	if !validExecutionDuration(config.LeaseDuration) || !validExecutionDuration(config.HeartbeatInterval) ||
		config.HeartbeatInterval >= config.LeaseDuration || !validExecutionDuration(config.IdlePollInterval) ||
		!validExecutionDuration(config.TurnTimeout) || !validExecutionDuration(config.CleanupTimeout) {
		return nil, fmt.Errorf("%w: timing", ErrInvalidExecutionWorker)
	}
	if config.ConcurrencyLimit <= 0 || config.ConcurrencyLimit > 10_000 {
		return nil, fmt.Errorf("%w: concurrency limit", ErrInvalidExecutionWorker)
	}
	developerProvider, err := copyProviderCredential(config.DeveloperProviderCredentialJSON)
	if err != nil {
		return nil, err
	}
	reviewerProvider, err := copyProviderCredential(config.ReviewerProviderCredentialJSON)
	if err != nil {
		zeroBytes(developerProvider)
		return nil, err
	}
	remoteBase, err := gitremote.ParseBaseURL(config.GitRemoteBaseURL)
	if err != nil {
		zeroBytes(developerProvider)
		zeroBytes(reviewerProvider)
		return nil, fmt.Errorf("%w: Git remote base URL", ErrInvalidExecutionWorker)
	}
	return &ExecutionWorker{
		store: dependencies.Store, developerCredentials: dependencies.DeveloperCredentials,
		reviewerCredentials: dependencies.ReviewerCredentials, defaultBranch: dependencies.DefaultBranch,
		launcher: dependencies.Launcher, sessions: dependencies.Sessions, outcomes: dependencies.Outcomes,
		workspace: dependencies.Workspace, claimOwner: config.ClaimOwner, leaseDuration: config.LeaseDuration,
		heartbeatInterval: config.HeartbeatInterval, idlePollInterval: config.IdlePollInterval,
		turnTimeout: config.TurnTimeout, cleanupTimeout: config.CleanupTimeout,
		concurrencyLimit: config.ConcurrencyLimit, developerProvider: developerProvider,
		reviewerProvider: reviewerProvider, gitRemoteBase: remoteBase, onError: config.OnError,
	}, nil
}

// ProcessNext atomically claims, executes, and durably settles at most one Agent Turn.
func (worker *ExecutionWorker) ProcessNext(ctx context.Context) (processed bool, err error) {
	lease, acquired, err := worker.store.ClaimAndAcquireAgentTurn(ctx, worker.claimOwner, worker.leaseDuration, worker.concurrencyLimit)
	if err != nil {
		return false, fmt.Errorf("claim and acquire Agent Turn: %w", err)
	}
	if !acquired {
		return false, nil
	}

	secrets := append(providerCredentialSecrets(worker.developerProvider), providerCredentialSecrets(worker.reviewerProvider)...)
	defer func() {
		if err != nil {
			err = sanitizeLaunchError(err, secrets)
		}
	}()
	heartbeat := worker.startHeartbeat(ctx, lease)
	if heartbeat.initialErr != nil {
		heartbeat.stop()
		return true, fmt.Errorf("heartbeat acquired Agent Turn: %w", heartbeat.initialErr)
	}
	released, operationErr := worker.execute(heartbeat.workCtx, heartbeat.leaseCtx, &heartbeat, &lease, &secrets)
	heartbeatErr := heartbeat.stop()
	if operationErr != nil {
		if heartbeatErr != nil && !released {
			return true, errors.Join(fmt.Errorf("heartbeat Agent Turn: %w", heartbeatErr), operationErr)
		}
		return true, operationErr
	}
	if heartbeatErr != nil && !released {
		return true, fmt.Errorf("heartbeat Agent Turn: %w", heartbeatErr)
	}
	return true, nil
}

func (worker *ExecutionWorker) execute(workCtx, leaseCtx context.Context, heartbeat *executionHeartbeat, lease *store.AgentTurnLease, secrets *[]string) (bool, error) {
	execution, err := worker.store.GetAgentTurnExecutionContext(workCtx, *lease)
	if err != nil {
		return false, fmt.Errorf("get fenced Agent Turn execution context: %w", err)
	}
	paths, pathsErr := worker.workspace.Paths(execution.Assignment.ID)
	operationErr := pathsErr
	if operationErr != nil {
		operationErr = fmt.Errorf("resolve Agent Turn workspace paths: %w", operationErr)
	}

	var repositoryCredential string
	var providerCredential json.RawMessage
	if operationErr == nil {
		switch execution.Assignment.Role {
		case workflow.RoleDeveloper:
			providerCredential = worker.developerProvider
			repositoryCredential, operationErr = worker.developerCredentials.RepositoryCredential(workCtx, execution.Repository.Owner, execution.Repository.Name)
		case workflow.RoleReviewer:
			providerCredential = worker.reviewerProvider
			repositoryCredential, operationErr = worker.reviewerCredentials.RepositoryCredential(workCtx, execution.Repository.Owner, execution.Repository.Name)
		default:
			operationErr = errors.New("Agent Turn has an invalid Role")
		}
		if repositoryCredential != "" {
			*secrets = append(*secrets, repositoryCredential)
		}
		if operationErr != nil {
			operationErr = fmt.Errorf("obtain Role repository credential: %w", operationErr)
		} else if strings.TrimSpace(repositoryCredential) == "" {
			operationErr = errors.New("Role repository credential is empty")
		}
	}

	var defaultBranch githubapi.DefaultBranch
	if operationErr == nil {
		defaultBranch, err = worker.defaultBranch.ResolveDefaultBranch(workCtx, repositoryCredential, execution.Repository.Owner, execution.Repository.Name)
		if err != nil {
			operationErr = fmt.Errorf("resolve repository default branch: %w", err)
		}
	}
	var repositoryURL string
	if operationErr == nil {
		repositoryURL, operationErr = worker.gitRemoteBase.RepositoryURL(execution.Repository.Owner, execution.Repository.Name)
		if operationErr != nil {
			operationErr = errors.New("construct secure repository URL: invalid repository identity")
		}
	}

	var runtime ExecutionRuntime
	if operationErr == nil {
		launchProviderCredential := append(json.RawMessage(nil), providerCredential...)
		runtime, err = worker.launcher.LaunchExecution(workCtx, LaunchRequest{
			Lease: *lease, LeaseDuration: worker.leaseDuration, RepositoryURL: repositoryURL,
			DefaultBranchName: defaultBranch.Name, DefaultBranchSHA: defaultBranch.CommitSHA,
			InitialFeatureBranch: fmt.Sprintf("omnigrex/issue-%d", execution.Issue.Number),
			RepositoryCredential: repositoryCredential, ProviderCredentialJSON: launchProviderCredential,
			PublishMCPRenewal: heartbeat.publishMCPRenewal,
		})
		zeroBytes(launchProviderCredential)
		if !nilDependency(runtime) {
			refreshed := runtime.CurrentLease()
			if !sameExecutionLease(*lease, refreshed) || refreshed.LeaseExpiresAt.IsZero() {
				operationErr = errors.New("launch Agent Turn Runtime Process: refreshed lease binding mismatch")
			} else {
				*lease = refreshed
			}
		}
		if err != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("launch Agent Turn Runtime Process: %w", err))
		} else if nilDependency(runtime) || nilDependency(runtime.PromptClient()) {
			operationErr = errors.Join(operationErr, errors.New("launch Agent Turn Runtime Process: launcher returned an invalid runtime"))
		}
	}

	if operationErr == nil {
		execution, err = worker.store.GetAgentTurnExecutionContext(workCtx, *lease)
		if err != nil {
			operationErr = fmt.Errorf("refresh fenced Agent Turn execution context: %w", err)
		}
	}
	if operationErr == nil {
		if err := worker.store.OpenMutationAdmission(workCtx, *lease); err != nil {
			operationErr = fmt.Errorf("open Agent Turn mutation admission: %w", err)
		}
	}

	var promptResponse *acp.PromptResponse
	if operationErr == nil {
		currentHead := defaultBranch.CommitSHA
		if execution.ChangeProposal != nil {
			currentHead = execution.ChangeProposal.HeadSHA
		}
		content, envelopeErr := BuildEventEnvelope(execution, currentHead)
		if envelopeErr != nil {
			operationErr = fmt.Errorf("build Agent Turn event envelope: %w", envelopeErr)
		} else {
			promptCtx, cancelPrompt := context.WithTimeout(workCtx, worker.turnTimeout)
			response, promptErr := worker.sessions.Prompt(promptCtx, session.PromptRequest{
				Lease: *lease, Session: execution.Session, Client: runtime.PromptClient(), Content: content,
			})
			cancelPrompt()
			if promptErr != nil {
				operationErr = fmt.Errorf("prompt Agent Turn: %w", promptErr)
			} else {
				promptResponse = &response
			}
		}
	}

	return worker.finalize(leaseCtx, *lease, execution, paths, repositoryCredential, runtime, promptResponse, operationErr)
}

func (worker *ExecutionWorker) finalize(leaseCtx context.Context, lease store.AgentTurnLease, execution store.AgentTurnExecutionContext, paths workspace.Paths, repositoryCredential string, runtime ExecutionRuntime, promptResponse *acp.PromptResponse, operationErr error) (bool, error) {
	if lostLease(leaseCtx, operationErr) {
		return false, errors.Join(operationErr, worker.cleanupWithoutFence(runtime))
	}

	closeErr := worker.retryFinalization(leaseCtx, func(ctx context.Context) error {
		return worker.store.CloseMutationAdmission(ctx, lease)
	})
	if closeErr != nil {
		return false, errors.Join(operationErr, closeErr, worker.cleanupWithoutFence(runtime))
	}
	drainRecoveryRequired, drainErr := worker.drainRuntime(leaseCtx, runtime)
	if drainErr != nil {
		return false, errors.Join(operationErr, wrapExecutionError("close and drain Agent Turn MCP authority", drainErr), worker.cleanupWithoutFence(runtime))
	}

	unsettled, listErr := worker.waitForRecoverableMutations(leaseCtx, lease, drainRecoveryRequired)
	if listErr != nil {
		return false, errors.Join(operationErr, fmt.Errorf("inspect unsettled Agent Turn mutations: %w", listErr), worker.cleanupWithoutFence(runtime))
	}

	cleanupErr := worker.cleanupWithRetry(leaseCtx, runtime)
	combinedErr := errors.Join(operationErr, wrapExecutionError("cleanup Agent Turn runtime", cleanupErr))
	if lostLease(leaseCtx, cleanupErr) {
		return false, combinedErr
	}
	if cleanupErr != nil || drainRecoveryRequired || len(unsettled) != 0 {
		recoveryErr := worker.beginAgentTurnRecovery(leaseCtx, lease)
		if recoveryErr != nil {
			return false, errors.Join(combinedErr, fmt.Errorf("begin Agent Turn recovery: %w", recoveryErr))
		}
		return true, combinedErr
	}
	if lostLease(leaseCtx, nil) {
		return false, combinedErr
	}

	reconciliation := OutcomeReconciliation{
		Lease: lease, Execution: execution, RepositoryCredential: repositoryCredential, Paths: paths,
	}
	if combinedErr == nil && promptResponse != nil {
		reconciliation.PromptResponse = promptResponse
	} else {
		classification := PromptErrorFailure
		if operationErr != nil {
			classification = ClassifyPromptError(operationErr)
		}
		reconciliation.PromptError = classification
		diagnosticSecrets := append(providerCredentialSecrets(worker.developerProvider), providerCredentialSecrets(worker.reviewerProvider)...)
		diagnosticSecrets = append(diagnosticSecrets, repositoryCredential)
		reconciliation.PromptDiagnostic = sanitizeLaunchError(combinedErr, diagnosticSecrets).Error()
	}
	var observation store.AgentTurnSettlementObservation
	reconcileErr := worker.retryBoundedFinalization(leaseCtx, func(ctx context.Context) error {
		var err error
		observation, err = worker.outcomes.Reconcile(ctx, reconciliation)
		return err
	})
	if errors.Is(reconcileErr, store.ErrAgentTurnMutationsUnsettled) {
		recoveryErr := worker.beginAgentTurnRecovery(leaseCtx, lease)
		return recoveryErr == nil, errors.Join(combinedErr, reconcileErr, recoveryErr)
	}
	if reconcileErr != nil {
		if lostLease(leaseCtx, reconcileErr) {
			return false, errors.Join(combinedErr, fmt.Errorf("reconcile Agent Turn outcome: %w", reconcileErr))
		}
		recoveryErr := worker.beginAgentTurnRecovery(leaseCtx, lease)
		return recoveryErr == nil, errors.Join(
			combinedErr,
			fmt.Errorf("reconcile Agent Turn outcome: %w", reconcileErr),
			wrapExecutionError("begin Agent Turn recovery", recoveryErr),
		)
	}
	settleErr := worker.retryBoundedFinalization(leaseCtx, func(ctx context.Context) error {
		_, err := worker.store.SettleAgentTurn(ctx, lease, observation)
		return err
	})
	if settleErr != nil {
		if lostLease(leaseCtx, settleErr) {
			return true, errors.Join(combinedErr, fmt.Errorf("settle Agent Turn: %w", settleErr))
		}
		recoveryErr := worker.beginAgentTurnRecovery(leaseCtx, lease)
		released := recoveryErr == nil || errors.Is(recoveryErr, store.ErrAgentTurnFenceLost)
		return released, errors.Join(
			combinedErr,
			fmt.Errorf("settle Agent Turn: %w", settleErr),
			wrapExecutionError("begin Agent Turn recovery", recoveryErr),
		)
	}
	return true, combinedErr
}

type executionHeartbeat struct {
	workCtx    context.Context
	leaseCtx   context.Context
	stopSignal context.CancelFunc
	done       <-chan error
	initialErr error
	renewal    *heartbeatRenewal
}

type heartbeatRenewal struct {
	mutex sync.RWMutex
	renew MCPRenewal
}

func (worker *ExecutionWorker) startHeartbeat(parent context.Context, lease store.AgentTurnLease) executionHeartbeat {
	workCtx, cancelWork := context.WithCancelCause(parent)
	leaseCtx, cancelLease := context.WithCancelCause(context.Background())
	heartbeatCtx, stopSignal := context.WithCancel(context.Background())
	done := make(chan error, 1)
	initial := make(chan error, 1)
	renewal := &heartbeatRenewal{}
	go func() {
		refresh := func() error {
			refreshed, err := worker.store.RefreshAgentTurnLease(heartbeatCtx, lease, worker.leaseDuration)
			if err != nil {
				return err
			}
			renewal.mutex.RLock()
			renew := renewal.renew
			renewal.mutex.RUnlock()
			if renew != nil && !renew(refreshed.LeaseExpiresAt) {
				return errors.New("renew MCP authority after Agent Turn heartbeat")
			}
			return nil
		}
		err := refresh()
		initial <- err
		if err == nil {
			ticker := time.NewTicker(worker.heartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatCtx.Done():
					err = nil
					goto finished
				case <-ticker.C:
					err = refresh()
					if err != nil {
						goto finished
					}
				}
			}
		}
	finished:
		if err != nil {
			cancelWork(err)
			cancelLease(err)
		}
		done <- err
	}()
	initialErr := <-initial
	if initialErr != nil {
		cancelWork(initialErr)
		cancelLease(initialErr)
	}
	return executionHeartbeat{workCtx: workCtx, leaseCtx: leaseCtx, stopSignal: stopSignal, done: done, initialErr: initialErr, renewal: renewal}
}

func (heartbeat *executionHeartbeat) publishMCPRenewal(renew MCPRenewal) {
	heartbeat.renewal.mutex.Lock()
	heartbeat.renewal.renew = renew
	heartbeat.renewal.mutex.Unlock()
}

func (heartbeat executionHeartbeat) stop() error {
	heartbeat.stopSignal()
	return <-heartbeat.done
}

func (worker *ExecutionWorker) cleanupWithoutFence(runtime ExecutionRuntime) error {
	if nilDependency(runtime) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), worker.cleanupTimeout)
	defer cancel()
	return runtime.Cleanup(ctx)
}

func (worker *ExecutionWorker) cleanupWithRetry(leaseCtx context.Context, runtime ExecutionRuntime) error {
	if nilDependency(runtime) {
		return nil
	}
	var cleanupErr error
	for attempt := 0; attempt < 2; attempt++ {
		cleanupErr = worker.withFinalizationTimeout(leaseCtx, runtime.Cleanup)
		if cleanupErr == nil || lostLease(leaseCtx, cleanupErr) {
			return cleanupErr
		}
		if attempt == 0 {
			if err := worker.waitForFinalizationRetry(leaseCtx); err != nil {
				return errors.Join(cleanupErr, err)
			}
		}
	}
	return cleanupErr
}

func (worker *ExecutionWorker) drainRuntime(leaseCtx context.Context, runtime ExecutionRuntime) (bool, error) {
	if nilDependency(runtime) {
		return false, nil
	}
	for {
		err := worker.withFinalizationTimeout(leaseCtx, runtime.CloseMCP)
		switch {
		case err == nil:
			return false, nil
		case lostLease(leaseCtx, err):
			return false, err
		case errors.Is(err, mcp.ErrMutationDrainUnresolved):
			return true, nil
		case !errors.Is(err, context.DeadlineExceeded):
			return false, err
		}
		if err := worker.waitForFinalizationRetry(leaseCtx); err != nil {
			return false, err
		}
	}
}

func (worker *ExecutionWorker) retryFinalization(leaseCtx context.Context, operation func(context.Context) error) error {
	for {
		err := worker.withFinalizationTimeout(leaseCtx, operation)
		if err == nil || lostLease(leaseCtx, err) {
			return err
		}
		if err := worker.waitForFinalizationRetry(leaseCtx); err != nil {
			return err
		}
	}
}

func (worker *ExecutionWorker) beginAgentTurnRecovery(leaseCtx context.Context, lease store.AgentTurnLease) error {
	return worker.retryFinalization(leaseCtx, func(ctx context.Context) error {
		_, err := worker.store.BeginAgentTurnRecovery(ctx, lease)
		return err
	})
}

func (worker *ExecutionWorker) retryBoundedFinalization(leaseCtx context.Context, operation func(context.Context) error) error {
	var operationErr error
	for attempt := 0; attempt < finalizationAttemptLimit; attempt++ {
		operationErr = worker.withFinalizationTimeout(leaseCtx, operation)
		if operationErr == nil || lostLease(leaseCtx, operationErr) || errors.Is(operationErr, store.ErrAgentTurnMutationsUnsettled) {
			return operationErr
		}
		if attempt+1 < finalizationAttemptLimit {
			if err := worker.waitForFinalizationRetry(leaseCtx); err != nil {
				return errors.Join(operationErr, err)
			}
		}
	}
	return operationErr
}

func (worker *ExecutionWorker) waitForRecoverableMutations(leaseCtx context.Context, lease store.AgentTurnLease, repairResidual bool) ([]store.MutationReservation, error) {
	for {
		var unsettled []store.MutationReservation
		err := worker.withFinalizationTimeout(leaseCtx, func(ctx context.Context) error {
			var err error
			unsettled, err = worker.store.ListUnsettledMutations(ctx, lease)
			return err
		})
		if err != nil {
			if lostLease(leaseCtx, err) {
				return nil, err
			}
		} else if len(unsettled) == 0 {
			return nil, nil
		} else if repairResidual {
			recoverable, repairErr := worker.repairResidualMutations(leaseCtx, lease, unsettled)
			if repairErr == nil {
				return recoverable, nil
			}
			if lostLease(leaseCtx, repairErr) {
				return nil, repairErr
			}
		} else if recoverableMutations(unsettled) {
			return unsettled, nil
		}
		if err := worker.waitForFinalizationRetry(leaseCtx); err != nil {
			return nil, err
		}
	}
}

func (worker *ExecutionWorker) repairResidualMutations(leaseCtx context.Context, lease store.AgentTurnLease, mutations []store.MutationReservation) ([]store.MutationReservation, error) {
	var recoverable []store.MutationReservation
	var repairErr error
	for _, mutation := range mutations {
		switch mutation.State {
		case store.MutationReserved:
			err := worker.withFinalizationTimeout(leaseCtx, func(ctx context.Context) error {
				return worker.store.FailMutation(ctx, lease, mutation.ID, errors.New("backend mutation was not started"))
			})
			repairErr = errors.Join(repairErr, err)
		case store.MutationInFlight:
			err := worker.withFinalizationTimeout(leaseCtx, func(ctx context.Context) error {
				return worker.store.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("backend mutation outcome was not durably finalized"))
			})
			if err == nil {
				mutation.State = store.MutationUnknown
				recoverable = append(recoverable, mutation)
			}
			repairErr = errors.Join(repairErr, err)
		case store.MutationUnknown, store.MutationReconciling:
			recoverable = append(recoverable, mutation)
		default:
			repairErr = errors.Join(repairErr, fmt.Errorf("unexpected unsettled mutation state %q", mutation.State))
		}
	}
	return recoverable, repairErr
}

func (worker *ExecutionWorker) waitForFinalizationRetry(leaseCtx context.Context) error {
	pause := worker.cleanupTimeout / 10
	if pause < time.Millisecond {
		pause = time.Millisecond
	}
	if pause > 100*time.Millisecond {
		pause = 100 * time.Millisecond
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-leaseCtx.Done():
		return context.Cause(leaseCtx)
	case <-timer.C:
		return nil
	}
}

func (worker *ExecutionWorker) withFinalizationTimeout(leaseCtx context.Context, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(leaseCtx, worker.cleanupTimeout)
	defer cancel()
	return operation(ctx)
}

// Run processes execution actions until its context ends.
func (worker *ExecutionWorker) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan error, worker.concurrencyLimit)
	var onErrorMutex sync.Mutex
	for range worker.concurrencyLimit {
		go func() {
			results <- worker.runLoop(runCtx, &onErrorMutex)
		}()
	}

	var result error
	for range worker.concurrencyLimit {
		err := <-results
		if result == nil && err != nil {
			result = err
			cancel()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return result
}

func (worker *ExecutionWorker) runLoop(ctx context.Context, onErrorMutex *sync.Mutex) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if worker.onError != nil {
				onErrorMutex.Lock()
				worker.onError(err)
				onErrorMutex.Unlock()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := waitForWorkerPoll(ctx, worker.idlePollInterval); err != nil {
				return err
			}
			continue
		}
		if processed {
			continue
		}
		if err := waitForWorkerPoll(ctx, worker.idlePollInterval); err != nil {
			return err
		}
	}
}

func copyProviderCredential(raw json.RawMessage) (json.RawMessage, error) {
	copy := append(json.RawMessage(nil), raw...)
	var object map[string]json.RawMessage
	if json.Unmarshal(copy, &object) != nil || len(object) == 0 {
		zeroBytes(copy)
		for key, value := range object {
			zeroBytes(value)
			delete(object, key)
		}
		return nil, fmt.Errorf("%w: provider credential must be a nonempty JSON object", ErrInvalidExecutionWorker)
	}
	for key, value := range object {
		zeroBytes(value)
		delete(object, key)
	}
	return copy, nil
}

func validExecutionDuration(value time.Duration) bool {
	return value >= time.Microsecond && value <= maximumWorkerDuration
}

func providerCredentialSecrets(raw json.RawMessage) []string {
	secrets := []string{string(raw)}
	var decoded any
	if json.Unmarshal(raw, &decoded) == nil {
		collectStringSecrets(decoded, &secrets)
	}
	return secrets
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func sameExecutionLease(first, second store.AgentTurnLease) bool {
	return first.ID == second.ID && first.ExecutionEpoch == second.ExecutionEpoch &&
		first.ControlRevision == second.ControlRevision && first.AgentAssignmentID == second.AgentAssignmentID &&
		first.AgentSessionID == second.AgentSessionID && first.OwnerID == second.OwnerID &&
		first.OwnerToken == second.OwnerToken && first.JobLease.ID == second.JobLease.ID &&
		first.JobLease.Attempt == second.JobLease.Attempt && first.JobLease.LeaseToken == second.JobLease.LeaseToken
}

func recoverableMutations(mutations []store.MutationReservation) bool {
	for _, mutation := range mutations {
		if mutation.State != store.MutationUnknown && mutation.State != store.MutationReconciling {
			return false
		}
	}
	return len(mutations) != 0
}

func lostLease(ctx context.Context, err error) bool {
	return context.Cause(ctx) != nil || errors.Is(err, store.ErrAgentTurnFenceLost)
}

func wrapExecutionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
