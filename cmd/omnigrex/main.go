package main

import (
	"context"
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
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/server"
	"github.com/jozala/omnigrex/internal/store"
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
		githubServices.webhookProcessor.Run,
		preparationWorker.Run,
	)
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
