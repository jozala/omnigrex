package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultDatabaseURL                = "postgres://omnigrex@postgres:5432/omnigrex?sslmode=disable"
	defaultDatabasePasswordSecretFile = "/run/secrets/omnigrex-database-password"
	defaultDockerAgentNetwork         = "omnigrex-agent"
	defaultWorkspaceVolume            = "omnigrex-workspaces"
	defaultRuntimeStateVolume         = "omnigrex-runtime-state"
	defaultMiseVolume                 = "omnigrex-mise"
	defaultAgentImageReference        = "omnigrex/opencode:1.18.19"
	defaultHTTPAddr                   = ":8080"
	defaultReadinessTimeout           = 15 * time.Second
	defaultShutdownTimeout            = 10 * time.Second
)

type Config struct {
	DatabaseURL                string
	DatabasePasswordSecretFile string
	DockerAgentNetwork         string
	WorkspaceVolume            string
	RuntimeStateVolume         string
	MiseVolume                 string
	AgentImageReference        string
	HTTPAddr                   string
	ReadinessTimeout           time.Duration
	ShutdownTimeout            time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	config := Config{
		DatabaseURL:                valueOrDefault(getenv("OMNIGREX_DATABASE_URL"), defaultDatabaseURL),
		DatabasePasswordSecretFile: valueOrDefault(getenv("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE"), defaultDatabasePasswordSecretFile),
		DockerAgentNetwork:         valueOrDefault(getenv("OMNIGREX_DOCKER_AGENT_NETWORK"), defaultDockerAgentNetwork),
		WorkspaceVolume:            valueOrDefault(getenv("OMNIGREX_WORKSPACE_VOLUME"), defaultWorkspaceVolume),
		RuntimeStateVolume:         valueOrDefault(getenv("OMNIGREX_RUNTIME_STATE_VOLUME"), defaultRuntimeStateVolume),
		MiseVolume:                 valueOrDefault(getenv("OMNIGREX_MISE_VOLUME"), defaultMiseVolume),
		AgentImageReference:        valueOrDefault(getenv("OMNIGREX_AGENT_IMAGE_REFERENCE"), defaultAgentImageReference),
		HTTPAddr:                   valueOrDefault(getenv("OMNIGREX_HTTP_ADDR"), defaultHTTPAddr),
		ReadinessTimeout:           defaultReadinessTimeout,
		ShutdownTimeout:            defaultShutdownTimeout,
	}

	databaseURL, err := url.Parse(config.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_URL must be a PostgreSQL URL")
	}
	if !filepath.IsAbs(config.DatabasePasswordSecretFile) {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE must be an absolute path")
	}
	for name, value := range map[string]string{
		"OMNIGREX_DOCKER_AGENT_NETWORK":  config.DockerAgentNetwork,
		"OMNIGREX_WORKSPACE_VOLUME":      config.WorkspaceVolume,
		"OMNIGREX_RUNTIME_STATE_VOLUME":  config.RuntimeStateVolume,
		"OMNIGREX_MISE_VOLUME":           config.MiseVolume,
		"OMNIGREX_AGENT_IMAGE_REFERENCE": config.AgentImageReference,
	} {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s must not be blank", name)
		}
	}

	if value := getenv("OMNIGREX_READINESS_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_READINESS_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_READINESS_TIMEOUT must be positive")
		}
		config.ReadinessTimeout = timeout
	}

	if value := getenv("OMNIGREX_SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_SHUTDOWN_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_SHUTDOWN_TIMEOUT must be positive")
		}
		config.ShutdownTimeout = timeout
	}

	return config, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
