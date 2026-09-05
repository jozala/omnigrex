package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	runtimesession "github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/server"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflowaction"
	"github.com/jozala/omnigrex/internal/workspace"
)

var version = "dev"

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
	githubServices, err := configureGitHub(settings, database, logger)
	if err != nil {
		return err
	}
	openCodeV1, err := runtimeprofile.NewOpenCodeV1(settings.OpenCodeACPV1Image, settings.OpenCodeACPV1Platform)
	if err != nil {
		return fmt.Errorf("configure opencode-acp/v1 Runtime Profile: %w", err)
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

	readiness := server.ReadinessFunc(func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, settings.ReadinessTimeout)
		defer cancel()
		return errors.Join(database.Ready(probeCtx), dockerProbe.Check(probeCtx))
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
		labelWorker.Run,
		humanHandoffWorker.Run,
		failureWorker.Run,
	)
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
