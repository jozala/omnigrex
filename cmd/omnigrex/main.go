package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	runtimesession "github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/server"
	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflowaction"
	"github.com/jozala/omnigrex/internal/workspace"
)

var version = "dev"

const (
	startupMaximumPasses           = 100
	startupMaximumRuntimeProcesses = 100_000
	startupMaximumRecoveries       = 100_000
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	settings, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("omnigrex starting", "version", version)
	if err := run(ctx, settings, logger); err != nil {
		logger.Error("omnigrex stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("omnigrex stopped")
}

func run(ctx context.Context, settings config.Config, logger *slog.Logger) error {
	startupCtx, startupCancel := context.WithTimeout(ctx, settings.ReadinessTimeout)
	database, err := store.Open(startupCtx, settings.DatabaseURL, settings.DatabasePasswordSecretFile)
	startupCancel()
	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	defer database.Close()
	openCodeV1, err := runtimeprofile.NewOpenCodeV1(settings.OpenCodeACPV1Image, settings.OpenCodeACPV1Platform)
	if err != nil {
		return fmt.Errorf("configure opencode-acp/v1 Runtime Profile: %w", err)
	}
	if err := importRuntimeProfileCompatibilityResults(ctx, database, settings.RuntimeProfileCompatibilityResultsFile, openCodeV1); err != nil {
		return fmt.Errorf("import Runtime Profile compatibility results: %w", err)
	}
	githubServices, err := configureGitHub(settings, database, logger)
	if err != nil {
		return err
	}
	runtimeRegistry, err := runtimeprofile.NewRegistry(openCodeV1)
	if err != nil {
		return fmt.Errorf("configure Runtime Profile Registry: %w", err)
	}
	developerRepositoryCredentials, err := githubapi.NewRepositoryInstallationCredentialProvider(
		githubServices.developerSigner, githubServices.api, githubapi.DeveloperAppPermissions(), nil,
	)
	if err != nil {
		return fmt.Errorf("configure Developer repository credentials: %w", err)
	}
	reviewerRepositoryCredentials, err := githubapi.NewRepositoryInstallationCredentialProvider(
		githubServices.reviewerSigner, githubServices.api, githubapi.ReviewerAppPermissions(), nil,
	)
	if err != nil {
		return fmt.Errorf("configure Reviewer repository credentials: %w", err)
	}
	preparer := agentturn.NewPreparer(agentprofile.NewLoader(githubServices.api), runtimeRegistry, database)
	preparationWorker, err := agentturn.NewWorker(database, developerRepositoryCredentials, reviewerRepositoryCredentials, preparer, agentturn.WorkerConfig{
		ClaimOwner:        githubServices.claimOwner + ":prepare-agent-turn",
		LeaseDuration:     settings.AgentTurnPreparationLeaseDuration,
		HeartbeatInterval: settings.AgentTurnPreparationHeartbeatInterval,
		IdlePollInterval:  settings.AgentTurnPreparationPollInterval,
		RetryDelay:        settings.AgentTurnPreparationRetryDelay,
		OnError: func(err error) {
			logger.Error("prepare Agent Turn", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure Agent Turn preparation Worker: %w", err)
	}
	workspaces, err := workspace.New(workspace.Options{
		WorkspaceRoot: settings.WorkspaceRoot, PublicationRoot: settings.WorkspaceRoot, MiseRoot: settings.MiseRoot,
	})
	if err != nil {
		return fmt.Errorf("configure Assignment workspaces: %w", err)
	}
	readLedger, err := mcp.NewStoreReadLedger(database)
	if err != nil {
		return fmt.Errorf("configure MCP read ledger: %w", err)
	}
	repositoryCredentials := roleRepositoryCredentials{
		developer: developerRepositoryCredentials,
		reviewer:  reviewerRepositoryCredentials,
	}
	toolBackend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub:           githubServices.api,
		Credentials:      repositoryCredentials,
		Publisher:        workspaces,
		Workflow:         mcp.LedgerWorkflowMutations{},
		GitRemoteBaseURL: settings.GitRemoteBaseURL,
	})
	if err != nil {
		return fmt.Errorf("configure MCP tool backend: %w", err)
	}
	toolGateway, err := mcp.New(mcp.Config{
		EndpointURL: settings.MCPEndpointURL, Store: database, Backend: toolBackend,
		Ledger: readLedger, LifecycleContext: ctx, MutationFinalizationTimeout: settings.AgentTurnExecutionCleanupTimeout,
		MutationOperationTimeout: settings.MCPMutationOperationTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure MCP Tool Gateway: %w", err)
	}
	mutationReconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{
		GitHub: githubServices.api, Credentials: repositoryCredentials, Publications: workspaces,
		GitRemoteBaseURL: settings.GitRemoteBaseURL,
	})
	if err != nil {
		return fmt.Errorf("configure MCP mutation reconciler: %w", err)
	}
	mutationRecoveryWorker, err := mcp.NewRecoveryWorker(database, mutationReconciler, mcp.RecoveryWorkerConfig{
		ClaimOwner:        githubServices.claimOwner + ":reconcile-agent-turn-mutations",
		LeaseDuration:     settings.AgentTurnPreparationLeaseDuration,
		HeartbeatInterval: settings.AgentTurnPreparationHeartbeatInterval,
		IdlePollInterval:  settings.AgentTurnPreparationPollInterval,
		RetryDelay:        settings.AgentTurnPreparationRetryDelay,
		OnError: func(err error) {
			logger.Error("reconcile Agent Turn mutations", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure Agent Turn mutation recovery Worker: %w", err)
	}
	runtimeCleaner, err := dockerruntime.NewExactLabelCleaner(dockerruntime.ExactLabelCleanerOptions{
		StopTimeout: settings.ShutdownTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure stale Runtime Process cleanup: %w", err)
	}
	defer func() {
		if err := runtimeCleaner.Close(); err != nil {
			logger.Error("close stale Runtime Process cleanup", "error", err)
		}
	}()
	runtimeInventory, err := dockerruntime.NewRuntimeProcessInventory()
	if err != nil {
		return fmt.Errorf("configure Runtime Process inventory: %w", err)
	}
	defer func() {
		if err := runtimeInventory.Close(); err != nil {
			logger.Error("close Runtime Process inventory", "error", err)
		}
	}()
	profileContract := openCodeV1.Contract()
	runtimeStateCleaner, err := dockerruntime.NewAssignmentRuntimeStateCleaner(dockerruntime.AssignmentRuntimeStateCleanerOptions{
		Image: profileContract.Image,
		Platform: dockerruntime.Platform{
			OS: profileContract.Platform.OS, Architecture: profileContract.Platform.Arch,
		},
		User: strconv.FormatUint(uint64(profileContract.User.UID), 10) + ":" +
			strconv.FormatUint(uint64(profileContract.User.GID), 10),
		RuntimeStateVolume: settings.RuntimeStateVolume,
	})
	if err != nil {
		return fmt.Errorf("configure Assignment runtime-state cleanup: %w", err)
	}
	defer func() {
		if err := runtimeStateCleaner.Close(); err != nil {
			logger.Error("close Assignment runtime-state cleanup", "error", err)
		}
	}()
	exactImages, err := dockerruntime.NewExactImageAvailability()
	if err != nil {
		return fmt.Errorf("configure exact Runtime Profile image availability: %w", err)
	}
	defer func() {
		if err := exactImages.Close(); err != nil {
			logger.Error("close exact Runtime Profile image availability", "error", err)
		}
	}()
	profileAvailability, err := runtimeprofile.NewAvailabilityChecker(database, runtimeRegistry.Catalog, exactImages)
	if err != nil {
		return fmt.Errorf("configure protected Runtime Profile availability: %w", err)
	}
	startupDocker := startupDockerAdapter{inventory: runtimeInventory, cleaner: runtimeCleaner}
	startupReconciler, err := startup.NewReconciler(database, startupDocker, startupDocker, startupDocker, startup.ReconcilerOptions{
		MaxPasses: startupMaximumPasses, MaxRuntimeProcesses: startupMaximumRuntimeProcesses,
		MaxRecoveries: startupMaximumRecoveries,
	})
	if err != nil {
		return fmt.Errorf("configure startup reconciliation: %w", err)
	}
	expiredTurnMonitor, err := startup.NewMonitor(database, startup.MonitorOptions{
		PollInterval: settings.AgentTurnPreparationPollInterval, MaxRecoveriesPerPoll: startupMaximumRecoveries,
		OnError: func(err error) { logger.Error("reconcile expired Agent Turns", "error", err) },
	})
	if err != nil {
		return fmt.Errorf("configure expired Agent Turn monitor: %w", err)
	}
	runtimeStopWorker, err := agentturn.NewStopWorker(database, runtimeCleaner, workspaces, agentturn.StopWorkerConfig{
		ClaimOwner:           githubServices.claimOwner + ":stop-stale-runtime",
		LeaseDuration:        settings.AgentTurnPreparationLeaseDuration,
		HeartbeatInterval:    settings.AgentTurnPreparationHeartbeatInterval,
		IdlePollInterval:     settings.AgentTurnPreparationPollInterval,
		CleanupRetryInterval: settings.AgentTurnPreparationRetryDelay,
		OnError: func(err error) {
			logger.Error("stop stale Runtime Process", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure stale Runtime Process stop Worker: %w", err)
	}
	sessions := runtimesession.NewCoordinator(database)
	runtimeLauncher, err := agentturn.NewLauncher(agentturn.LauncherConfig{
		Store: database, Registry: runtimeRegistry, Workspace: workspaces, Gateway: toolGateway,
		Docker: agentturn.ProductionDockerFactory{}, ACP: agentturn.ProductionACPFactory{},
		Sessions: sessions, Network: settings.DockerAgentNetwork,
		WorkspaceVolume: settings.WorkspaceVolume, RuntimeStateVolume: settings.RuntimeStateVolume,
		MiseVolume: settings.MiseVolume, ACPOptions: acp.ClientOptions{},
	})
	if err != nil {
		return fmt.Errorf("configure Runtime Process Launcher: %w", err)
	}
	outcomeReconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: database, GitHub: githubServices.api,
	})
	if err != nil {
		return fmt.Errorf("configure Agent Turn outcome reconciler: %w", err)
	}
	developerProviderCredentialJSON, err := readNonemptyJSONObject(settings.DeveloperProviderCredentialsFile)
	if err != nil {
		return fmt.Errorf("read Developer provider credentials: %w", err)
	}
	reviewerProviderCredentialJSON, err := readNonemptyJSONObject(settings.ReviewerProviderCredentialsFile)
	if err != nil {
		zeroBytes(developerProviderCredentialJSON)
		return fmt.Errorf("read Reviewer provider credentials: %w", err)
	}
	executionWorker, executionWorkerErr := agentturn.NewExecutionWorker(agentturn.ExecutionWorkerDependencies{
		Store: database, DeveloperCredentials: developerRepositoryCredentials,
		ReviewerCredentials: reviewerRepositoryCredentials, DefaultBranch: githubServices.api,
		Launcher: runtimeLauncher, Sessions: sessions, Outcomes: outcomeReconciler, Workspace: workspaces,
	}, agentturn.ExecutionWorkerConfig{
		ClaimOwner: githubServices.claimOwner + ":execute-agent-turn", LeaseDuration: settings.AgentTurnExecutionLeaseDuration,
		HeartbeatInterval: settings.AgentTurnExecutionHeartbeatInterval, IdlePollInterval: settings.AgentTurnExecutionPollInterval,
		TurnTimeout: settings.AgentTurnExecutionTurnTimeout, CleanupTimeout: settings.AgentTurnExecutionCleanupTimeout,
		ConcurrencyLimit: settings.AgentTurnConcurrencyLimit, DeveloperProviderCredentialJSON: developerProviderCredentialJSON,
		ReviewerProviderCredentialJSON: reviewerProviderCredentialJSON, GitRemoteBaseURL: settings.GitRemoteBaseURL,
		OnError: func(err error) {
			logger.Error("execute Agent Turn", "error", err)
		},
	})
	zeroBytes(developerProviderCredentialJSON)
	zeroBytes(reviewerProviderCredentialJSON)
	if executionWorkerErr != nil {
		return fmt.Errorf("configure Agent Turn execution Worker: %w", executionWorkerErr)
	}
	reconciliationWorker, err := webhook.NewReconciliationWorker(database, githubServices.webhookProcessor, webhook.ReconciliationWorkerConfig{
		ClaimOwner: githubServices.claimOwner + ":reconcile-pending-events", LeaseDuration: settings.WorkflowEffectLeaseDuration,
		HeartbeatInterval: settings.WorkflowEffectHeartbeatInterval, IdlePollInterval: settings.WorkflowEffectPollInterval,
		RetryDelay: settings.WorkflowEffectRetryDelay,
		OnError: func(err error) {
			logger.Error("reconcile pending Workflow events", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure pending-event reconciliation Worker: %w", err)
	}
	labelWorker, err := githubapi.NewLabelWorker(database, developerRepositoryCredentials, githubServices.api, githubapi.VisibleEffectWorkerConfig{
		ClaimOwner: githubServices.claimOwner + ":reconcile-github-labels", LeaseDuration: settings.WorkflowEffectLeaseDuration,
		HeartbeatInterval: settings.WorkflowEffectHeartbeatInterval, IdlePollInterval: settings.WorkflowEffectPollInterval,
		RetryDelay: settings.WorkflowEffectRetryDelay,
		OnError: func(err error) {
			logger.Error("reconcile GitHub Workflow labels", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure GitHub label Worker: %w", err)
	}
	humanHandoffWorker, err := githubapi.NewHumanHandoffWorker(database, developerRepositoryCredentials, githubServices.api, githubapi.VisibleEffectWorkerConfig{
		ClaimOwner: githubServices.claimOwner + ":publish-human-handoff", LeaseDuration: settings.WorkflowEffectLeaseDuration,
		HeartbeatInterval: settings.WorkflowEffectHeartbeatInterval, IdlePollInterval: settings.WorkflowEffectPollInterval,
		RetryDelay: settings.WorkflowEffectRetryDelay,
		OnError: func(err error) {
			logger.Error("publish GitHub Human Handoff", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure GitHub Human Handoff Worker: %w", err)
	}
	failureWorker, err := workflowaction.NewFailureWorker(database, workflowaction.FailureWorkerConfig{
		ClaimOwner:    githubServices.claimOwner + ":escalate-workflow-action-failure",
		LeaseDuration: settings.WorkflowEffectLeaseDuration, IdlePollInterval: settings.WorkflowEffectPollInterval,
		OnError: func(err error) {
			logger.Error("escalate Workflow action failure", "error", err)
		},
	})
	if err != nil {
		return fmt.Errorf("configure Workflow action failure Worker: %w", err)
	}
	closureWorkerConfig := func(owner, operation string) workflowaction.ClosureWorkerConfig {
		return workflowaction.ClosureWorkerConfig{
			ClaimOwner:    githubServices.claimOwner + ":" + owner,
			LeaseDuration: settings.WorkflowEffectLeaseDuration, HeartbeatInterval: settings.WorkflowEffectHeartbeatInterval,
			IdlePollInterval: settings.WorkflowEffectPollInterval, RetryDelay: settings.WorkflowEffectRetryDelay,
			OnError: func(err error) { logger.Error(operation, "error", err) },
		}
	}
	closureStopWorker, err := workflowaction.NewClosureStopWorker(database, runtimeCleaner,
		closureWorkerConfig("stop-closing-agent-turn", "stop closing Agent Turn"))
	if err != nil {
		return fmt.Errorf("configure closure Agent Turn stop Worker: %w", err)
	}
	closureSettlementWorker, err := workflowaction.NewClosureSettlementWorker(database, mutationReconciler,
		closureWorkerConfig("settle-workflow-closure", "settle Workflow closure"))
	if err != nil {
		return fmt.Errorf("configure Workflow closure settlement Worker: %w", err)
	}
	retentionWorker, err := retention.NewWorker(database, runtimeStateCleaner, retention.WorkerConfig{
		ClaimOwner:    githubServices.claimOwner + ":collect-retained-assignments",
		LeaseDuration: settings.WorkflowEffectLeaseDuration, HeartbeatInterval: settings.WorkflowEffectHeartbeatInterval,
		IdlePollInterval: settings.WorkflowEffectPollInterval, RetryDelay: settings.WorkflowEffectRetryDelay,
		OnError: func(err error) { logger.Error("collect retained Assignments", "error", err) },
	})
	if err != nil {
		return fmt.Errorf("configure Assignment retention Worker: %w", err)
	}

	reconciliation, err := prepareStartup(ctx, settings.ReadinessTimeout, profileAvailability, startupReconciler)
	if err != nil {
		return err
	}
	logger.Info("startup reconciliation complete",
		"passes", reconciliation.Passes,
		"recovered_agent_turns", reconciliation.RecoveredAgentTurns,
		"removed_runtime_identities", reconciliation.RemovedRuntimeIdentities,
		"removed_malformed_processes", reconciliation.RemovedMalformedProcesses,
	)

	dockerProbe, err := dockerruntime.NewReadinessProbe(dockerruntime.ReadinessProbeOptions{
		AgentNetwork:       settings.DockerAgentNetwork,
		WorkspaceVolume:    settings.WorkspaceVolume,
		RuntimeStateVolume: settings.RuntimeStateVolume,
		MiseVolume:         settings.MiseVolume,
		AgentImage:         settings.AgentImageReference,
	})
	if err != nil {
		return fmt.Errorf("initialize Docker readiness: %w", err)
	}
	defer func() {
		if err := dockerProbe.Close(); err != nil {
			logger.Error("close Docker readiness", "error", err)
		}
	}()

	readiness := combinedReadiness(settings.ReadinessTimeout, database.Ready, dockerProbe.Check,
		profileAvailability.Check, func(ctx context.Context) error {
			return database.CheckPendingRuntimeProfileCompatibility(ctx, openCodeV1)
		})
	return runServices(ctx,
		func(ctx context.Context) error {
			return server.Run(ctx, settings.HTTPAddr, settings.ShutdownTimeout, logger, readiness, githubServices.webhookHandler)
		},
		func(ctx context.Context) error {
			return server.RunHandler(ctx, settings.MCPAddr, settings.ShutdownTimeout, logger, "mcp", toolGateway)
		},
		githubServices.webhookProcessor.Run,
		reconciliationWorker.Run,
		preparationWorker.Run,
		executionWorker.Run,
		runtimeStopWorker.Run,
		mutationRecoveryWorker.Run,
		expiredTurnMonitor.Run,
		labelWorker.Run,
		humanHandoffWorker.Run,
		failureWorker.Run,
		closureStopWorker.Run,
		closureSettlementWorker.Run,
		retentionWorker.Run,
	)
}

type runtimeProcessInventory interface {
	List(context.Context) (dockerruntime.RuntimeProcessInventorySnapshot, error)
}

type startupRuntimeCleaner interface {
	EnsureAbsent(context.Context, map[string]string) error
	EnsureManagedContainerAbsent(context.Context, string) error
}

type startupDockerAdapter struct {
	inventory runtimeProcessInventory
	cleaner   startupRuntimeCleaner
}

func (adapter startupDockerAdapter) List(ctx context.Context) (startup.RuntimeInventorySnapshot, error) {
	dockerSnapshot, err := adapter.inventory.List(ctx)
	if err != nil {
		return startup.RuntimeInventorySnapshot{}, err
	}
	snapshot := startup.RuntimeInventorySnapshot{
		Processes:  make([]startup.ManagedRuntimeProcess, 0, len(dockerSnapshot.Processes)),
		Duplicates: make([]startup.DuplicateManagedRuntimeProcess, 0, len(dockerSnapshot.Duplicates)),
		Malformed:  make([]startup.MalformedManagedRuntimeProcess, 0, len(dockerSnapshot.Malformed)),
	}
	for _, process := range dockerSnapshot.Processes {
		identity, ok := startupRuntimeIdentity(process.Identity)
		if !ok {
			snapshot.Malformed = append(snapshot.Malformed, startup.MalformedManagedRuntimeProcess{
				ContainerID: process.ContainerID, Reason: string(dockerruntime.MalformedExecutionEpoch),
			})
			continue
		}
		snapshot.Processes = append(snapshot.Processes, startup.ManagedRuntimeProcess{
			Identity: identity, ContainerID: process.ContainerID,
		})
	}
	for _, duplicate := range dockerSnapshot.Duplicates {
		identity, ok := startupRuntimeIdentity(duplicate.Identity)
		if !ok {
			for _, containerID := range duplicate.ContainerIDs {
				snapshot.Malformed = append(snapshot.Malformed, startup.MalformedManagedRuntimeProcess{
					ContainerID: containerID, Reason: string(dockerruntime.MalformedExecutionEpoch),
				})
			}
			continue
		}
		snapshot.Duplicates = append(snapshot.Duplicates, startup.DuplicateManagedRuntimeProcess{
			Identity: identity, ContainerIDs: append([]string(nil), duplicate.ContainerIDs...),
		})
	}
	for _, malformed := range dockerSnapshot.Malformed {
		snapshot.Malformed = append(snapshot.Malformed, startup.MalformedManagedRuntimeProcess{
			ContainerID: malformed.ContainerID, Reason: string(malformed.Reason),
		})
	}
	return snapshot, nil
}

func (adapter startupDockerAdapter) EnsureAbsent(ctx context.Context, identity store.AgentTurnRuntimeIdentity) error {
	return adapter.cleaner.EnsureAbsent(ctx, map[string]string{
		dockerruntime.RuntimeProcessAssignmentLabel: identity.AssignmentID,
		dockerruntime.RuntimeProcessSessionLabel:    identity.AgentSessionID,
		dockerruntime.RuntimeProcessTurnLabel:       identity.AgentTurnID,
		dockerruntime.RuntimeProcessEpochLabel:      strconv.FormatInt(identity.ExecutionEpoch, 10),
		dockerruntime.RuntimeProfileIdentityLabel:   identity.RuntimeProfileName + "/" + identity.RuntimeProfileVersion,
	})
}

func (adapter startupDockerAdapter) EnsureMalformedAbsent(ctx context.Context, process startup.MalformedManagedRuntimeProcess) error {
	return adapter.cleaner.EnsureManagedContainerAbsent(ctx, process.ContainerID)
}

func startupRuntimeIdentity(identity dockerruntime.RuntimeProcessIdentity) (store.AgentTurnRuntimeIdentity, bool) {
	if identity.ExecutionEpoch > math.MaxInt64 {
		return store.AgentTurnRuntimeIdentity{}, false
	}
	return store.AgentTurnRuntimeIdentity{
		AssignmentID: identity.AssignmentID, AgentSessionID: identity.AgentSessionID,
		AgentTurnID: identity.AgentTurnID, ExecutionEpoch: int64(identity.ExecutionEpoch),
		RuntimeProfileName: identity.RuntimeProfile.Name, RuntimeProfileVersion: identity.RuntimeProfile.Version,
	}, true
}

type protectedBindingChecker interface {
	CheckBindings(context.Context) error
}

type startupTurnReconciler interface {
	Reconcile(context.Context) (startup.ReconciliationResult, error)
}

func prepareStartup(ctx context.Context, timeout time.Duration, bindings protectedBindingChecker, reconciler startupTurnReconciler) (startup.ReconciliationResult, error) {
	startupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := bindings.CheckBindings(startupCtx); err != nil {
		return startup.ReconciliationResult{}, fmt.Errorf("validate protected Runtime Profile bindings during startup: %w", err)
	}
	result, err := reconciler.Reconcile(startupCtx)
	if err != nil {
		return result, fmt.Errorf("reconcile durable runtime state during startup: %w", err)
	}
	return result, nil
}

func combinedReadiness(timeout time.Duration, checks ...func(context.Context) error) server.ReadinessFunc {
	return func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		results := make([]error, 0, len(checks))
		for _, check := range checks {
			results = append(results, check(probeCtx))
		}
		return errors.Join(results...)
	}
}

type roleRepositoryCredentials struct {
	developer *githubapi.RepositoryInstallationCredentialProvider
	reviewer  *githubapi.RepositoryInstallationCredentialProvider
}

func (credentials roleRepositoryCredentials) DeveloperCredential(ctx context.Context, repository mcp.RepositoryScope) (string, error) {
	return credentials.developer.RepositoryCredential(ctx, repository.Owner, repository.Name)
}

func (credentials roleRepositoryCredentials) ReviewerCredential(ctx context.Context, repository mcp.RepositoryScope) (string, error) {
	return credentials.reviewer.RepositoryCredential(ctx, repository.Owner, repository.Name)
}

type configuredGitHub struct {
	api              *githubapi.APIClient
	developerSigner  *githubapi.AppJWTSigner
	reviewerSigner   *githubapi.AppJWTSigner
	webhookHandler   *webhook.Handler
	webhookProcessor *webhook.Processor
	claimOwner       string
}

func configureGitHub(settings config.Config, database *store.Store, logger *slog.Logger) (*configuredGitHub, error) {
	developerKey, err := os.ReadFile(settings.GitHubDeveloperPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Developer GitHub App private key: %w", err)
	}
	developerSigner, err := githubapi.NewAppJWTSigner(settings.GitHubDeveloperAppID, developerKey, nil)
	if err != nil {
		return nil, fmt.Errorf("configure Developer GitHub App: %w", err)
	}
	reviewerKey, err := os.ReadFile(settings.GitHubReviewerPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Reviewer GitHub App private key: %w", err)
	}
	reviewerSigner, err := githubapi.NewAppJWTSigner(settings.GitHubReviewerAppID, reviewerKey, nil)
	if err != nil {
		return nil, fmt.Errorf("configure Reviewer GitHub App: %w", err)
	}
	if developerSigner.PublicKeyFingerprint() == reviewerSigner.PublicKeyFingerprint() {
		return nil, errors.New("Developer and Reviewer GitHub Apps must use distinct private keys")
	}
	api, err := githubapi.NewAPIClient(&http.Client{Timeout: 15 * time.Second}, settings.GitHubAPIURL)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub API: %w", err)
	}
	webhookSecret, err := readSecret(settings.GitHubWebhookSecretFile)
	if err != nil {
		return nil, fmt.Errorf("read GitHub webhook secret: %w", err)
	}
	handler, err := webhook.NewHandler(webhookSecret, database)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub webhook handler: %w", err)
	}
	claimOwner, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("identify webhook processor: %w", err)
	}
	processor, err := webhook.NewProcessor(database, webhook.ProcessorConfig{
		ClaimOwner:                  claimOwner,
		LeaseDuration:               settings.WebhookLeaseDuration,
		IdlePollInterval:            settings.WebhookPollInterval,
		AssignmentRetentionDuration: settings.AssignmentRetentionDuration,
		OnError: func(err error) {
			logger.Error("process GitHub webhook delivery", "error", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure GitHub webhook processor: %w", err)
	}
	return &configuredGitHub{
		api: api, developerSigner: developerSigner, reviewerSigner: reviewerSigner,
		webhookHandler: handler, webhookProcessor: processor, claimOwner: claimOwner,
	}, nil
}

func readSecret(path string) ([]byte, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := strings.TrimRight(string(contents), "\r\n")
	if secret == "" {
		return nil, errors.New("secret is empty")
	}
	return []byte(secret), nil
}

func readNonemptyJSONObject(path string) (json.RawMessage, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(contents, &object); err != nil || len(object) == 0 {
		zeroBytes(contents)
		for key, value := range object {
			zeroBytes(value)
			delete(object, key)
		}
		return nil, errors.New("file must contain a nonempty JSON object")
	}
	for key, value := range object {
		zeroBytes(value)
		delete(object, key)
	}
	return json.RawMessage(contents), nil
}

type runtimeProfileCompatibilityImporter interface {
	ImportRuntimeProfileCompatibilityResults(context.Context, runtimeprofile.CompatibilityResultsFile) error
}

func importRuntimeProfileCompatibilityResults(ctx context.Context, importer runtimeProfileCompatibilityImporter, filePath string, target runtimeprofile.Profile) error {
	if filePath == "" {
		return nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open compatibility results file: %w", err)
	}
	defer file.Close()
	results, err := runtimeprofile.DecodeCompatibilityResultsFile(file)
	if err != nil {
		return err
	}
	targetContract := target.Contract()
	for index, result := range results.Results {
		if result.Target != target.Binding() || result.Platform != targetContract.Platform {
			return fmt.Errorf("compatibility result %d does not target the configured Runtime Profile binding and platform", index)
		}
	}
	if err := importer.ImportRuntimeProfileCompatibilityResults(ctx, results); err != nil {
		return err
	}
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func runServices(ctx context.Context, services ...func(context.Context) error) error {
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsFound := make(chan error, len(services))
	for _, service := range services {
		go func() { errorsFound <- service(serviceCtx) }()
	}

	first := <-errorsFound
	cancel()
	results := []error{serviceError(first)}
	for range len(services) - 1 {
		results = append(results, serviceError(<-errorsFound))
	}
	return errors.Join(results...)
}

func serviceError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func healthcheck() int {
	url := os.Getenv("OMNIGREX_HEALTHCHECK_URL")
	if url == "" {
		url = "http://127.0.0.1:8080/healthz"
	}

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(os.Stderr, "healthcheck returned %s\n", response.Status)
		return 1
	}
	return 0
}
