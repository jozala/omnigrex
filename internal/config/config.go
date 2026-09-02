package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
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
	defaultGitHubAPIURL               = "https://api.github.com"
	defaultHTTPAddr                   = ":8080"
	defaultReadinessTimeout           = 15 * time.Second
	defaultShutdownTimeout            = 10 * time.Second
	defaultWebhookLeaseDuration       = 30 * time.Second
	defaultWebhookPollInterval        = 250 * time.Millisecond
	minimumWebhookLeaseDuration       = 5 * time.Second
)

type Config struct {
	DatabaseURL                   string
	DatabasePasswordSecretFile    string
	DockerAgentNetwork            string
	WorkspaceVolume               string
	RuntimeStateVolume            string
	MiseVolume                    string
	AgentImageReference           string
	GitHubAPIURL                  string
	GitHubDeveloperAppID          int64
	GitHubReviewerAppID           int64
	GitHubDeveloperPrivateKeyFile string
	GitHubReviewerPrivateKeyFile  string
	GitHubWebhookSecretFile       string
	WebhookLeaseDuration          time.Duration
	WebhookPollInterval           time.Duration
	HTTPAddr                      string
	ReadinessTimeout              time.Duration
	ShutdownTimeout               time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	config := Config{
		DatabaseURL:                   valueOrDefault(getenv("OMNIGREX_DATABASE_URL"), defaultDatabaseURL),
		DatabasePasswordSecretFile:    valueOrDefault(getenv("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE"), defaultDatabasePasswordSecretFile),
		DockerAgentNetwork:            valueOrDefault(getenv("OMNIGREX_DOCKER_AGENT_NETWORK"), defaultDockerAgentNetwork),
		WorkspaceVolume:               valueOrDefault(getenv("OMNIGREX_WORKSPACE_VOLUME"), defaultWorkspaceVolume),
		RuntimeStateVolume:            valueOrDefault(getenv("OMNIGREX_RUNTIME_STATE_VOLUME"), defaultRuntimeStateVolume),
		MiseVolume:                    valueOrDefault(getenv("OMNIGREX_MISE_VOLUME"), defaultMiseVolume),
		AgentImageReference:           valueOrDefault(getenv("OMNIGREX_AGENT_IMAGE_REFERENCE"), defaultAgentImageReference),
		GitHubAPIURL:                  valueOrDefault(getenv("OMNIGREX_GITHUB_API_URL"), defaultGitHubAPIURL),
		GitHubDeveloperPrivateKeyFile: getenv("OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE"),
		GitHubReviewerPrivateKeyFile:  getenv("OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE"),
		GitHubWebhookSecretFile:       getenv("OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE"),
		WebhookLeaseDuration:          defaultWebhookLeaseDuration,
		WebhookPollInterval:           defaultWebhookPollInterval,
		HTTPAddr:                      valueOrDefault(getenv("OMNIGREX_HTTP_ADDR"), defaultHTTPAddr),
		ReadinessTimeout:              defaultReadinessTimeout,
		ShutdownTimeout:               defaultShutdownTimeout,
	}

	databaseURL, err := url.Parse(config.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_URL must be a PostgreSQL URL")
	}
	if !filepath.IsAbs(config.DatabasePasswordSecretFile) {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE must be an absolute path")
	}
	githubAPIURL, err := url.Parse(config.GitHubAPIURL)
	if err != nil || githubAPIURL.Scheme != "https" || githubAPIURL.Host == "" || githubAPIURL.RawQuery != "" || githubAPIURL.Fragment != "" {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_API_URL must be an HTTPS URL without query or fragment")
	}
	config.GitHubDeveloperAppID, err = positiveInt64(getenv("OMNIGREX_GITHUB_DEVELOPER_APP_ID"))
	if err != nil {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_DEVELOPER_APP_ID must be a positive integer")
	}
	config.GitHubReviewerAppID, err = positiveInt64(getenv("OMNIGREX_GITHUB_REVIEWER_APP_ID"))
	if err != nil {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_REVIEWER_APP_ID must be a positive integer")
	}
	if config.GitHubDeveloperAppID == config.GitHubReviewerAppID {
		return Config{}, fmt.Errorf("Developer and Reviewer GitHub App IDs must be distinct")
	}
	for name, path := range map[string]string{
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE": config.GitHubDeveloperPrivateKeyFile,
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":  config.GitHubReviewerPrivateKeyFile,
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":        config.GitHubWebhookSecretFile,
	} {
		if !filepath.IsAbs(path) {
			return Config{}, fmt.Errorf("%s must be an absolute path", name)
		}
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

	if value := getenv("OMNIGREX_WEBHOOK_LEASE_DURATION"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_WEBHOOK_LEASE_DURATION: %w", err)
		}
		if duration < minimumWebhookLeaseDuration {
			return Config{}, fmt.Errorf("OMNIGREX_WEBHOOK_LEASE_DURATION must be at least %s", minimumWebhookLeaseDuration)
		}
		config.WebhookLeaseDuration = duration
	}

	if value := getenv("OMNIGREX_WEBHOOK_POLL_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_WEBHOOK_POLL_INTERVAL: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_WEBHOOK_POLL_INTERVAL must be positive")
		}
		config.WebhookPollInterval = duration
	}

	return config, nil
}

func positiveInt64(value string) (int64, error) {
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != value {
		return 0, fmt.Errorf("not a canonical positive integer")
	}
	return number, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
