package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
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
	return server.Run(ctx, settings.HTTPAddr, settings.ShutdownTimeout, logger, readiness)
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
